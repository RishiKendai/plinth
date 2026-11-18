// Package apierr defines the error type that maps to the API error envelope.
package apierr

import "net/http"

type Error struct {
	Status  int               `json:"-"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func New(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func Validation(fields map[string]string) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
		Message: "Some fields need attention.", Fields: fields}
}

var (
	ErrUnauthorized = New(http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
	ErrNotFound     = New(http.StatusNotFound, "not_found", "That item was not found.")
	ErrBadRequest   = New(http.StatusBadRequest, "bad_request", "The request could not be read.")
	ErrRateLimited  = New(http.StatusTooManyRequests, "rate_limited", "Too many requests. Wait a moment and try again.")
	ErrInternal     = New(http.StatusInternalServerError, "internal", "Something went wrong on our side. Nothing was changed. Try again.")
)
