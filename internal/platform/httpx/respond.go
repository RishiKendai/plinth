// Package httpx holds HTTP conventions: response envelope, error mapping, middleware.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"plinth/server/internal/platform/apierr"
)

// Success bodies are {"data": ...}. Error bodies are {"error": {code, message, fields?}}.
type envelope struct {
	Data any `json:"data"`
}
type errEnvelope struct {
	Error *apierr.Error `json:"error"`
}

func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Data: data})
}

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error writes err using the error envelope. Unknown errors become a generic 500 and are logged, never echoed.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		slog.ErrorContext(r.Context(), "unhandled error", "err", err.Error(), "request_id", RequestID(r.Context()))
		ae = apierr.ErrInternal
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if ae.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	w.WriteHeader(ae.Status)
	_ = json.NewEncoder(w).Encode(errEnvelope{Error: ae})
}

// Decode reads exactly one JSON object, rejecting unknown fields and trailing data.
func Decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return apierr.New(http.StatusRequestEntityTooLarge, "too_large", "The request is too large.")
		}
		return apierr.New(http.StatusBadRequest, "bad_json", "The request body is not valid.")
	}
	if _, err := dec.Token(); err != io.EOF {
		return apierr.New(http.StatusBadRequest, "bad_json", "The request body is not valid.")
	}
	return nil
}
