package httpapi

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
)

func asDeniedError(err error, target **ratelimit.DeniedError) bool {
	return errors.As(err, target)
}

func itoaHeader(value int) string {
	return strconv.Itoa(value)
}

const terminalHeader = "X-Cellar-Terminal-ID"

type quotaErrorBody struct {
	Code             string `json:"code"`
	Detail           string `json:"detail"`
	Terminal         string `json:"terminal"`
	Category         string `json:"category"`
	RetryAfterSeconds int   `json:"retry_after_seconds"`
	WindowStart      string `json:"window_start"`
	WindowEnd        string `json:"window_end"`
	Limit            int    `json:"limit"`
	Used             int    `json:"used"`
}

type quotaErrorResponse struct {
	Error quotaErrorBody `json:"error"`
}

// guarded runs a mutating handler behind the per-terminal quota for category.
// The quota verdict is produced before any body is read or business logic is
// called, so a rejected request never touches business state.
func guarded(limiter *ratelimit.Limiter, category ratelimit.Category,
	handler http.HandlerFunc,
) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		terminal, err := terminalIdentity(request)
		if err != nil {
			writeJSON(response, http.StatusBadRequest, errorResponse{
				Error: errorBody{
					Code:   "invalid_input",
					Detail: err.Error(),
					Field:  terminalHeader,
				},
			})
			return
		}

		decision, err := limiter.Allow(terminal, category)
		var denied *ratelimit.DeniedError
		if err != nil {
			if !asDeniedError(err, &denied) {
				// Local quota store failure: fail closed without invoking
				// business logic or touching business state.
				writeJSON(response, http.StatusServiceUnavailable, errorResponse{
					Error: errorBody{
						Code:   "rate_limit_unavailable",
						Detail: "local quota store is unavailable; the write was not processed",
					},
				})
				return
			}
		}

		writeQuotaHeaders(response, decision)
		if denied != nil {
			response.Header().Set("Retry-After", itoaHeader(denied.RetryAfterSeconds()))
			writeJSON(response, http.StatusTooManyRequests, quotaErrorResponse{
				Error: quotaErrorBody{
					Code:              ratelimit.CodeRateLimited,
					Detail:            denied.Error(),
					Terminal:          decision.Terminal,
					Category:          string(decision.Category),
					RetryAfterSeconds: denied.RetryAfterSeconds(),
					WindowStart:       decision.WindowStart.Format("2006-01-02T15:04:05Z07:00"),
					WindowEnd:         decision.WindowEnd.Format("2006-01-02T15:04:05Z07:00"),
					Limit:             decision.Limit,
					Used:              decision.Used,
				},
			})
			return
		}
		handler(response, request)
	}
}

func writeQuotaHeaders(response http.ResponseWriter, decision ratelimit.Decision) {
	header := response.Header()
	header.Set("X-RateLimit-Limit", itoaHeader(decision.Limit))
	header.Set("X-RateLimit-Remaining", itoaHeader(decision.Remaining))
	header.Set("X-RateLimit-Reset", itoaHeader(int(decision.WindowEnd.Unix())))
}

// terminalIdentity resolves the cellar terminal for a request. An explicit
// header wins so tablets behind one network address are distinguishable;
// otherwise the peer host (without port) identifies the terminal.
func terminalIdentity(request *http.Request) (string, error) {
	if value := strings.TrimSpace(request.Header.Get(terminalHeader)); value != "" {
		if err := ratelimit.ValidateTerminal(value); err != nil {
			return "", err
		}
		return value, nil
	}
	host := request.RemoteAddr
	if parsed, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		host = parsed
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("request has no identifiable terminal address")
	}
	if err := ratelimit.ValidateTerminal(host); err != nil {
		return "", err
	}
	return host, nil
}
