// Package httpjson writes JSON responses and OpenAI-shaped error bodies. It is
// the one spelling of both, so every handler answers with the same bytes.
package httpjson

import (
	"encoding/json"
	"net/http"

	"github.com/janit/viiwork/v2/meshapi"
)

// Write sends v as JSON with the given status. The body ends in a newline, as
// json.Encoder writes it.
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Error sends an OpenAI-compatible error body: {"error":{"message":…,"type":…}}.
func Error(w http.ResponseWriter, status int, typ, message string) {
	Write(w, status, meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: message, Type: typ}})
}

// ErrorCode is Error with a machine-readable code beside the type:
// {"error":{"message":…,"type":…,"code":…}}.
func ErrorCode(w http.ResponseWriter, status int, typ, code, message string) {
	Write(w, status, meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: message, Type: typ, Code: code}})
}
