// Package httpx holds the HTTP envelope, error mapping and middleware.
//
// The response envelope is identical to the Node backend's
// {statusCode, success, message, data|error} so the frontend needs no changes.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/hireonai/backend-hireon-go/internal/domain"
)

type Envelope struct {
	StatusCode int    `json:"statusCode"`
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
}

// errorLabel mirrors ResponseAPI.error() in the Node backend, including the
// generic "Error" fallback for statuses outside the four it special-cased.
func errorLabel(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Unauthorized"
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusNotFound:
		return "Not Found"
	case http.StatusInternalServerError:
		return "Internal Server Error"
	case http.StatusBadRequest:
		return "Bad Request"
	default:
		return "Error"
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Success writes the standard success envelope.
func Success(w http.ResponseWriter, data any, message string) {
	writeJSON(w, http.StatusOK, Envelope{
		StatusCode: http.StatusOK,
		Success:    true,
		Message:    message,
		Data:       data,
	})
}

// Error writes the standard failure envelope. The caller-supplied message is
// client-safe; internal causes are logged, never returned.
func Error(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, Envelope{
		StatusCode: status,
		Success:    false,
		Message:    message,
		Error:      errorLabel(status),
	})
}

// Fail maps any error to an envelope. Services only ever return sentinel errors
// or *domain.APIError, so this is the single place that knows about status codes.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	message := "Internal Server Error"

	var apiErr *domain.APIError
	switch {
	case errors.As(err, &apiErr):
		status = apiErr.Status
		message = apiErr.Message
	case errors.Is(err, domain.ErrNotFound):
		status, message = http.StatusNotFound, "Resource not found."
	case errors.Is(err, domain.ErrUnauthorized):
		status, message = http.StatusUnauthorized, "Invalid or expired token."
	case errors.Is(err, domain.ErrForbidden):
		status, message = http.StatusForbidden, "Forbidden to access."
	case errors.Is(err, domain.ErrConflict):
		status, message = http.StatusConflict, "Resource already exists."
	case errors.Is(err, domain.ErrValidation):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, domain.ErrUpstreamSlow):
		status, message = http.StatusGatewayTimeout, "Upstream service timed out."
	case errors.Is(err, domain.ErrUpstream):
		status, message = http.StatusBadGateway, "Upstream service error."
	}

	if status >= 500 {
		slog.Error("request failed",
			"method", r.Method, "path", r.URL.Path,
			"status", status, "error", err, "request_id", RequestID(r.Context()))
	}
	Error(w, status, message)
}

// BadRequest reproduces the Joi validation message the Node backend emitted, so
// clients relying on that text keep working.
func BadRequest(w http.ResponseWriter, message string) {
	Error(w, http.StatusBadRequest, message)
}
