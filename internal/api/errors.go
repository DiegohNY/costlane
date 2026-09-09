package api

import (
	"encoding/json"
	"net/http"
)

// The error envelope mirrors OpenAI's, so a client SDK parses failures from
// costlane with no changes at all.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Param   *string `json:"param"`
}

// Error types, chosen so a client can act on them without parsing prose.
const (
	ErrorTypeInvalidRequest  = "invalid_request_error"
	ErrorTypeInvalidAPIKey   = "invalid_api_key"
	ErrorTypeBudgetExceeded  = "budget_exceeded"
	ErrorTypeModelNotFound   = "model_not_found"
	ErrorTypeModelNotPriced  = "model_not_priced"
	ErrorTypeModelNotAllowed = "model_not_allowed"
	ErrorTypeUpstreamTimeout = "upstream_timeout"
	ErrorTypeInternal        = "internal_error"
)

// WriteError renders a failure in the OpenAI envelope.
//
// Callers pass a message they have already decided is safe to show: nothing
// here inspects it, so a credential must never reach this function in the
// first place.
func WriteError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	// Errors can carry budget state and key identity, so they must not be
	// stored by an intermediary.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{
		Message: message,
		Type:    errType,
		Code:    errType,
	}})
}

// WriteJSON renders a successful response.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
