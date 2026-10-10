// Package handler adapts HTTP requests to service calls. It owns validation,
// status codes and response shaping; services know nothing about HTTP.
package handler

import (
	"encoding/json"
	"mime/multipart"
	"net/http"

	"github.com/hireonai/backend-hireon-go/internal/httpx"
)

// decodeJSON reads a JSON body with a size cap so a large body cannot exhaust
// memory. The Node backend relied on Joi validation but had no body limit.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		httpx.BadRequest(w, "Invalid request payload input")
		return false
	}
	return true
}

// multipartFile extracts one file part with a size cap applied before the body is
// buffered. The Node routes used hapi multipart with maxBytes; this is the
// equivalent guard, applied at the transport boundary.
func multipartFile(r *http.Request, field string) (multipart.File, *multipart.FileHeader, error) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, nil, err
	}
	return r.FormFile(field)
}
