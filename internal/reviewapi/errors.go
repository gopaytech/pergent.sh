package reviewapi

import (
	"encoding/json"
	"net/http"
)

// The error codes of v1. A new one is a /v2/ change: a caller that switches on
// the code must never meet one it does not know.
const (
	CodeUnauthorized         = "unauthorized"
	CodeNotFound             = "not_found"
	CodeInvalid              = "invalid"
	CodeTooLarge             = "too_large"
	CodeRateLimited          = "rate_limited"
	CodeBudgetUsedUp         = "budget_used_up"
	CodeCannotReview         = "cannot_review"
	CodeNotFinished          = "not_finished"
	CodeIdempotencyKeyReused = "idempotency_key_reused"
)

// ErrorBody is the body of every error. A 5xx has an empty code: it is not a
// refusal a caller can act on.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON answers with v as JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorBody{Code: code, Message: message})
}

func writeInternal(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "", "internal error")
}
