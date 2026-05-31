// Package httpx contains small HTTP helpers used by handlers.
package httpx

import (
	"encoding/json"
	"net/http"
)

type ErrorResponse struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// WriteJSON encodes v as JSON with the given HTTP status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a structured error response with no extra context.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorResponse{Code: code, Message: message})
}

// WriteErrorDetails writes a structured error with machine-readable
// context fields. Use this for quota / size / capacity errors where the
// client wants to render "you need N more bytes" rather than just "no".
func WriteErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	WriteJSON(w, status, ErrorResponse{Code: code, Message: message, Details: details})
}
