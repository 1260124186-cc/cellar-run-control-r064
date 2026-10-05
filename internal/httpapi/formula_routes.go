package httpapi

import (
	"net/http"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

type createFormulaRequest struct {
	Name                  string  `json:"name"`
	Style                 string  `json:"style"`
	TargetOriginalGravity float64 `json:"target_original_gravity"`
	TargetFinalGravity    float64 `json:"target_final_gravity"`
	MaxFermentationTempC  float64 `json:"max_fermentation_temp_c"`
	MinimumDays           int     `json:"minimum_days"`
	MaximumDays           int     `json:"maximum_days"`
}

type setStepsRequest struct {
	Steps []domain.FormulaStep `json:"steps"`
}

func registerFormulaRoutes(mux *http.ServeMux, app *service.Service, limiter *ratelimit.Limiter) {
	mux.HandleFunc("POST /v1/formulas", guarded(limiter, ratelimit.CategoryFormula, createFormula(app)))
	// Read-only routes are never blocked by a terminal quota.
	mux.HandleFunc("GET /v1/formulas", listFormulas(app))
	mux.HandleFunc("GET /v1/formulas/{id}", getFormula(app))
	mux.HandleFunc("PUT /v1/formulas/{id}/steps", guarded(limiter, ratelimit.CategoryFormula, setFormulaSteps(app)))
	mux.HandleFunc("POST /v1/formulas/{id}/approve", guarded(limiter, ratelimit.CategoryFormula, approveFormula(app)))
	mux.HandleFunc("POST /v1/formulas/{id}/retire", guarded(limiter, ratelimit.CategoryFormula, retireFormula(app)))
}

func createFormula(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[createFormulaRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.CreateFormula(service.CreateFormulaInput{
			Name:                  body.Name,
			Style:                 body.Style,
			TargetOriginalGravity: body.TargetOriginalGravity,
			TargetFinalGravity:    body.TargetFinalGravity,
			MaxFermentationTempC:  body.MaxFermentationTempC,
			MinimumDays:           body.MinimumDays,
			MaximumDays:           body.MaximumDays,
		})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusCreated, formula)
	}
}

func listFormulas(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		formulas, err := app.ListFormulas()
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"formulas": formulas})
	}
}

func getFormula(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		formula, err := app.GetFormula(request.PathValue("id"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	}
}

func setFormulaSteps(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[setStepsRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.SetFormulaSteps(request.PathValue("id"),
			service.SetFormulaStepsInput{ExpectedVersion: expected, Steps: body.Steps})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	}
}

func approveFormula(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.ApproveFormula(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	}
}

func retireFormula(app *service.Service) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.RetireFormula(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	}
}
