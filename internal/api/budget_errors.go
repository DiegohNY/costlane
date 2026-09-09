package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/DiegohNY/costlane/internal/store"
)

// budgetErrorEnvelope extends the OpenAI error shape with the budget state.
//
// Whoever receives a 402 wants to know how much room is left and when the
// window resets. The diagnostic query has already read those numbers, so
// returning them costs nothing and saves the caller a round trip to an
// endpoint they may not have access to.
type budgetErrorEnvelope struct {
	Error budgetErrorBody `json:"error"`
}

type budgetErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Param   *string `json:"param"`

	LimitUSD       *string   `json:"budget_limit_usd"`
	SpentUSD       string    `json:"budget_spent_usd"`
	ReservedUSD    string    `json:"budget_reserved_usd"`
	WindowResetsAt time.Time `json:"window_resets_at"`
}

// WriteReserveError renders a reserve failure with the right status.
//
// 402 rather than 429 for an exhausted budget: retrying does not help,
// because a budget does not free itself until the window rolls over. A 429
// would make well-behaved SDKs retry a request that cannot succeed.
func WriteReserveError(w http.ResponseWriter, err error) {
	var exceeded *store.BudgetExceededError
	switch {
	case errors.As(err, &exceeded):
		writeBudgetExceeded(w, exceeded)
	case errors.Is(err, store.ErrKeyNotFound), errors.Is(err, store.ErrKeyRevoked):
		// Unknown and revoked are the same answer to the client: telling
		// them apart would confirm which keys exist.
		WriteError(w, http.StatusUnauthorized, ErrorTypeInvalidAPIKey,
			"the supplied credential is not valid")
	case errors.Is(err, store.ErrModelNotAllowed):
		WriteError(w, http.StatusForbidden, ErrorTypeModelNotAllowed,
			"this key is not permitted to use the requested model")
	default:
		WriteError(w, http.StatusInternalServerError, ErrorTypeInternal,
			"the request could not be authorised")
	}
}

func writeBudgetExceeded(w http.ResponseWriter, e *store.BudgetExceededError) {
	body := budgetErrorBody{
		Message: "the budget for this key is exhausted; it resets at the start of the next UTC month",
		Type:    ErrorTypeBudgetExceeded,
		Code:    ErrorTypeBudgetExceeded,

		SpentUSD:       e.SpentUSD.String(),
		ReservedUSD:    e.ReservedUSD.String(),
		WindowResetsAt: e.WindowResetsAt,
	}
	if e.LimitUSD != nil {
		limit := e.LimitUSD.String()
		body.LimitUSD = &limit
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(budgetErrorEnvelope{Error: body})
}
