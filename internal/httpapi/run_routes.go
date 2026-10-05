package httpapi

import (
	"net/http"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

type createRunRequest struct {
	Code           string  `json:"code"`
	FormulaID      string  `json:"formula_id"`
	PlannedVolumeL float64 `json:"planned_volume_liters"`
	Notes          string  `json:"notes"`
}

type reserveRunRequest struct {
	VesselID string `json:"vessel_id"`
}

type observationRequest struct {
	Sequence     int       `json:"sequence"`
	Gravity      float64   `json:"gravity"`
	TemperatureC float64   `json:"temperature_c"`
	ObservedAt   time.Time `json:"observed_at"`
	Note         string    `json:"note"`
}

func registerRunRoutes(mux *http.ServeMux, app *service.Service, limiter *ratelimit.Limiter) {
	mux.HandleFunc("POST /v1/runs", guarded(limiter, ratelimit.CategoryRun, createRun(app)))
	// Read-only routes are never blocked by a terminal quota.
	mux.HandleFunc("GET /v1/runs", listRuns(app))
	mux.HandleFunc("GET /v1/runs/{id}", getRun(app))
	mux.HandleFunc("POST /v1/runs/{id}/reserve", guarded(limiter, ratelimit.CategoryRun, reserveRun(app)))
	mux.HandleFunc("POST /v1/runs/{id}/start", guarded(limiter, ratelimit.CategoryRun, startRun(app)))
	mux.HandleFunc("POST /v1/runs/{id}/observations",
		guarded(limiter, ratelimit.CategoryObservation, appendObservation(app)))
	mux.HandleFunc("POST /v1/runs/{id}/conditioning", guarded(limiter, ratelimit.CategoryRun, beginConditioning(app)))
	mux.HandleFunc("POST /v1/runs/{id}/complete", guarded(limiter, ratelimit.CategoryRun, completeRun(app)))
	mux.HandleFunc("POST /v1/runs/{id}/abort", guarded(limiter, ratelimit.CategoryRun, abortRun(app)))
}

func createRun(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[createRunRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		run, err := app.CreateRun(service.CreateRunInput{
			Code:           body.Code,
			FormulaID:      body.FormulaID,
			PlannedVolumeL: body.PlannedVolumeL,
			Notes:          body.Notes,
		})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusCreated, run)
	}
}

func listRuns(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		runs, err := app.ListRuns()
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"runs": runs})
	}
}

func getRun(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		run, err := app.GetRun(request.PathValue("id"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, run)
	}
}

func reserveRun(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[reserveRunRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		run, vessel, err := app.ReserveRun(request.PathValue("id"), service.ReserveRunInput{
			ExpectedVersion: expected,
			VesselID:        body.VesselID,
		})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"run": run, "vessel": vessel})
	}
}

func startRun(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		run, vessel, err := app.StartRun(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"run": run, "vessel": vessel})
	}
}

func appendObservation(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[observationRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		run, err := app.AppendObservation(request.PathValue("id"), service.ObservationInput{
			Sequence:     body.Sequence,
			Gravity:      body.Gravity,
			TemperatureC: body.TemperatureC,
			ObservedAt:   body.ObservedAt,
			Note:         body.Note,
		})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, run)
	}
}

func beginConditioning(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		run, err := app.BeginConditioning(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, run)
	}
}

func completeRun(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		run, vessel, err := app.CompleteRun(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"run": run, "vessel": vessel})
	}
}

func abortRun(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		run, vessel, err := app.AbortRun(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"run": run, "vessel": vessel})
	}
}
