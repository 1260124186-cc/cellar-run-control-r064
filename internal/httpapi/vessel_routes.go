package httpapi

import (
	"net/http"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

type createVesselRequest struct {
	Code      string  `json:"code"`
	CapacityL float64 `json:"capacity_liters"`
}

func registerVesselRoutes(mux *http.ServeMux, app *service.Service, limiter *ratelimit.Limiter) {
	mux.HandleFunc("POST /v1/vessels", guarded(limiter, ratelimit.CategoryVessel, createVessel(app)))
	// Read-only routes are never blocked by a terminal quota.
	mux.HandleFunc("GET /v1/vessels", listVessels(app))
	mux.HandleFunc("GET /v1/vessels/{id}", getVessel(app))
	mux.HandleFunc("POST /v1/vessels/{id}/finish-cleaning",
		guarded(limiter, ratelimit.CategoryVessel, finishVesselCleaning(app)))
	mux.HandleFunc("POST /v1/vessels/{id}/retire",
		guarded(limiter, ratelimit.CategoryVessel, retireVessel(app)))
}

func createVessel(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[createVesselRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		vessel, err := app.CreateVessel(service.CreateVesselInput{
			Code:      body.Code,
			CapacityL: body.CapacityL,
		})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusCreated, vessel)
	}
}

func listVessels(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		vessels, err := app.ListVessels()
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"vessels": vessels})
	}
}

func getVessel(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		vessel, err := app.GetVessel(request.PathValue("id"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, vessel)
	}
}

func finishVesselCleaning(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		vessel, err := app.FinishCleaning(request.PathValue("id"),
			service.FinishCleaningInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, vessel)
	}
}

func retireVessel(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		vessel, err := app.RetireVessel(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, vessel)
	}
}
