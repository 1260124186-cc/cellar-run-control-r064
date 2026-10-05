package domain

import (
	"errors"
	"fmt"
)

type Code string

const (
	CodeInvalidInput        Code = "invalid_input"
	CodeFormulaNotFound     Code = "formula_not_found"
	CodeFormulaNameConflict Code = "formula_name_conflict"
	CodeFormulaState        Code = "invalid_formula_state"
	CodeFormulaInUse        Code = "formula_in_use"
	CodeVesselNotFound      Code = "vessel_not_found"
	CodeVesselConflict      Code = "vessel_code_conflict"
	CodeVesselUnavailable   Code = "vessel_unavailable"
	CodeVesselState         Code = "invalid_vessel_state"
	CodeRunNotFound         Code = "run_not_found"
	CodeRunConflict         Code = "run_code_conflict"
	CodeRunState            Code = "invalid_run_state"
	CodeObservationConflict Code = "observation_conflict"
	CodeRevisionConflict    Code = "revision_conflict"
	CodeQuotaExceeded       Code = "quota_exceeded"
	CodeStorage             Code = "storage_error"
)

type Error struct {
	Code    Code
	Message string
	Field   string
}

func (e *Error) Error() string {
	if e.Field == "" {
		return string(e.Code) + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Field)
}

func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func FieldError(code Code, field, message string) *Error {
	return &Error{Code: code, Message: message, Field: field}
}

func IsCode(err error, code Code) bool {
	var domainErr *Error
	return errors.As(err, &domainErr) && domainErr.Code == code
}
