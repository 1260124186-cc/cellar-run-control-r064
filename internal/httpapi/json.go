package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

const maxRequestBody = 1 << 20

type errorBody struct {
	Code   domain.Code `json:"code"`
	Detail string      `json:"detail"`
	Field  string      `json:"field,omitempty"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

func decodeJSON[T any](response http.ResponseWriter, request *http.Request) (T, error) {
	var value T
	if request.Body == nil {
		return value, domain.NewError(domain.CodeInvalidInput, "request body is required")
	}
	body := http.MaxBytesReader(response, request.Body, maxRequestBody)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, decodeError(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, domain.NewError(domain.CodeInvalidInput,
				"request body must contain one JSON object")
		}
		return value, decodeError(err)
	}
	return value, nil
}

func decodeError(err error) error {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return domain.NewError(domain.CodeInvalidInput, "request body exceeds 1 MiB")
	}
	return domain.FieldError(domain.CodeInvalidInput, "body",
		"must be valid JSON with known fields")
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeDomainError(response http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := errorBody{
		Code:   domain.CodeStorage,
		Detail: "internal service failure",
	}
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		body.Code = domainErr.Code
		body.Detail = domainErr.Message
		body.Field = domainErr.Field
		status = statusForCode(domainErr.Code)
	}
	writeJSON(response, status, errorResponse{Error: body})
}

func statusForCode(code domain.Code) int {
	switch code {
	case domain.CodeInvalidInput:
		return http.StatusBadRequest
	case domain.CodeFormulaNotFound, domain.CodeVesselNotFound, domain.CodeRunNotFound:
		return http.StatusNotFound
	case domain.CodeFormulaNameConflict, domain.CodeFormulaState,
		domain.CodeFormulaInUse, domain.CodeVesselConflict,
		domain.CodeVesselUnavailable, domain.CodeVesselState,
		domain.CodeRunConflict, domain.CodeRunState,
		domain.CodeObservationConflict:
		return http.StatusConflict
	case domain.CodeRevisionConflict:
		return http.StatusPreconditionFailed
	case domain.CodeQuotaExceeded:
		return http.StatusTooManyRequests
	case domain.CodeStorage:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func expectedVersion(request *http.Request) (*int64, error) {
	value := request.Header.Get("X-Expected-Version")
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return nil, domain.FieldError(domain.CodeInvalidInput,
			"X-Expected-Version", "must be a positive integer")
	}
	return &parsed, nil
}

func handleDecode(response http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	writeDomainError(response, err)
	return false
}
