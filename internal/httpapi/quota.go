package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
)

// TerminalHeader carries the cellar terminal identifier assigned to a
// workshop tablet or script. The service trusts this local-network header;
// when it is absent or malformed the peer address identifies the terminal.
const TerminalHeader = "X-Cellar-Terminal"

var terminalPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// withQuota gates mutating routes before they reach decoding or business
// logic. Health, quota-status, and every read-only request are never gated:
// a rejected request must not be able to touch business state or slow the
// shared store, while operators must always be able to inspect what happened.
func withQuota(limiter *ratelimit.Limiter, next http.Handler) http.Handler {
	if limiter == nil {
		return next
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		category, limited := categoryFor(request.Method, request.URL.Path)
		if !limited {
			next.ServeHTTP(response, request)
			return
		}
		terminal := resolveTerminal(request)
		decision, err := limiter.Allow(terminal, category)
		if err != nil {
			writeJSON(response, http.StatusInternalServerError, errorResponse{
				Error: errorBody{
					Code:   domain.CodeStorage,
					Detail: "quota state could not be recorded; request was not executed",
				},
			})
			return
		}
		response.Header().Set("X-RateLimit-Limit", strconv.Itoa(decision.Limit))
		response.Header().Set("X-RateLimit-Remaining", strconv.Itoa(decision.Remaining))
		if !decision.Allowed {
			writeQuotaExceeded(response, decision)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func registerQuotaRoutes(mux *http.ServeMux, limiter *ratelimit.Limiter) {
	mux.HandleFunc("GET /quota-status", func(response http.ResponseWriter, request *http.Request) {
		writeJSON(response, http.StatusOK, limiter.Status(resolveTerminal(request)))
	})
}

// categoryFor maps mutating routes to their operation category. Read methods
// and non-business paths return false so they stay ungated.
func categoryFor(method, path string) (ratelimit.Category, bool) {
	if method != http.MethodPost && method != http.MethodPut {
		return "", false
	}
	if !strings.HasPrefix(path, "/v1/") {
		return "", false
	}
	switch {
	case strings.HasPrefix(path, "/v1/formulas"):
		return ratelimit.CategoryFormula, true
	case strings.HasPrefix(path, "/v1/vessels"):
		return ratelimit.CategoryVessel, true
	case strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/observations"):
		return ratelimit.CategoryObservation, true
	case strings.HasPrefix(path, "/v1/runs"):
		return ratelimit.CategoryRun, true
	default:
		return "", false
	}
}

func resolveTerminal(request *http.Request) string {
	if header := strings.TrimSpace(request.Header.Get(TerminalHeader)); header != "" &&
		terminalPattern.MatchString(header) {
		return header
	}
	if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil && host != "" {
		return strings.Trim(host, "[]")
	}
	return request.RemoteAddr
}

func writeQuotaExceeded(response http.ResponseWriter, decision ratelimit.Decision) {
	seconds := int((decision.RetryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	response.Header().Set("Retry-After", strconv.Itoa(seconds))
	response.Header().Set("X-RateLimit-Reset", strconv.FormatInt(decision.BlockUntil.Unix(), 10))
	detail := fmt.Sprintf(
		"write quota for %s operations from terminal %q was exceeded; wait %d second(s) before retrying",
		decision.Category, decision.Terminal, seconds)
	if decision.Restored {
		detail = "block restored after controller restart: " + detail
	}
	writeJSON(response, http.StatusTooManyRequests, map[string]any{
		"error": map[string]any{
			"code":   domain.CodeQuotaExceeded,
			"detail": detail,
		},
		"terminal":            decision.Terminal,
		"category":            string(decision.Category),
		"retry_after_seconds": seconds,
		"blocked_until":       decision.BlockUntil,
		"restart_restored":    decision.Restored,
	})
}
