package httpapi

import (
	"net/http"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
)

type quotaView struct {
	Category      string `json:"category"`
	Limit         int    `json:"limit"`
	WindowSeconds int    `json:"window_seconds"`
}

type updateQuotaRequest struct {
	Limit         *int `json:"limit"`
	WindowSeconds *int `json:"window_seconds"`
}

// registerQuotaRoutes exposes the local quota configuration. These routes are
// not business writes and are not themselves rate limited: a throttled
// terminal must always be able to discover why it is waiting.
func registerQuotaRoutes(mux *http.ServeMux, limiter *ratelimit.Limiter) {
	mux.HandleFunc("GET /v1/quotas", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]any{"quotas": quotaViews(limiter.Quotas())})
	})

	mux.HandleFunc("PUT /v1/quotas/{category}", func(response http.ResponseWriter, request *http.Request) {
		category := ratelimit.Category(request.PathValue("category"))
		if !ratelimit.KnownCategory(category) {
			writeJSON(response, http.StatusBadRequest, errorResponse{
				Error: errorBody{
					Code:   "invalid_input",
					Detail: "category must be one of formula, vessel, run, observation",
					Field:  "category",
				},
			})
			return
		}

		current := limiter.Quotas()[category]
		body, err := decodeJSON[updateQuotaRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		updated := current
		if body.Limit != nil {
			updated.Limit = *body.Limit
		}
		if body.WindowSeconds != nil {
			updated.WindowSeconds = *body.WindowSeconds
		}
		if err := limiter.UpdateQuota(category, updated); err != nil {
			writeJSON(response, http.StatusBadRequest, errorResponse{
				Error: errorBody{
					Code:   "invalid_input",
					Detail: err.Error(),
					Field:  "quota",
				},
			})
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"quota": quotaView{
			Category:      string(category),
			Limit:         updated.Limit,
			WindowSeconds: updated.WindowSeconds,
		}})
	})
}

func quotaViews(quotas map[ratelimit.Category]ratelimit.Quota) []quotaView {
	views := make([]quotaView, 0, len(quotas))
	for _, category := range ratelimit.Categories() {
		quota := quotas[category]
		views = append(views, quotaView{
			Category:      string(category),
			Limit:         quota.Limit,
			WindowSeconds: quota.WindowSeconds,
		})
	}
	return views
}
