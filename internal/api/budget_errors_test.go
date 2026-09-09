package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/api"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/shopspring/decimal"
)

// A 402 must tell the caller where they stand, because the alternative is
// guessing or polling an endpoint they may not be allowed to read.
func TestBudgetExceededCarriesTheBudgetState(t *testing.T) {
	limit := decimal.RequireFromString("10")
	resets := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	rec := httptest.NewRecorder()
	api.WriteReserveError(rec, &store.BudgetExceededError{
		LimitUSD:       &limit,
		SpentUSD:       decimal.RequireFromString("9.5"),
		ReservedUSD:    decimal.RequireFromString("0.75"),
		WindowResetsAt: resets,
	})

	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402: retrying cannot help, so 429 would be wrong", rec.Code)
	}

	var body struct {
		Error struct {
			Type           string    `json:"type"`
			LimitUSD       *string   `json:"budget_limit_usd"`
			SpentUSD       string    `json:"budget_spent_usd"`
			ReservedUSD    string    `json:"budget_reserved_usd"`
			WindowResetsAt time.Time `json:"window_resets_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body, err)
	}
	if body.Error.Type != "budget_exceeded" {
		t.Errorf("type = %q, want budget_exceeded", body.Error.Type)
	}
	if body.Error.LimitUSD == nil || *body.Error.LimitUSD != "10" {
		t.Errorf("budget_limit_usd = %v, want 10", body.Error.LimitUSD)
	}
	if body.Error.SpentUSD != "9.5" {
		t.Errorf("budget_spent_usd = %q, want 9.5", body.Error.SpentUSD)
	}
	if body.Error.ReservedUSD != "0.75" {
		t.Errorf("budget_reserved_usd = %q, want 0.75", body.Error.ReservedUSD)
	}
	if !body.Error.WindowResetsAt.Equal(resets) {
		t.Errorf("window_resets_at = %s, want %s", body.Error.WindowResetsAt, resets)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: this body carries budget state", got)
	}
}

// An unlimited key that somehow reaches this path has no limit to report,
// and null must not become zero.
func TestBudgetExceededWithNoLimitReportsNull(t *testing.T) {
	rec := httptest.NewRecorder()
	api.WriteReserveError(rec, &store.BudgetExceededError{
		SpentUSD:       decimal.Zero,
		ReservedUSD:    decimal.Zero,
		WindowResetsAt: time.Now().Add(time.Hour),
	})
	var body struct {
		Error struct {
			LimitUSD *string `json:"budget_limit_usd"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Error.LimitUSD != nil {
		t.Errorf("budget_limit_usd = %v, want null", body.Error.LimitUSD)
	}
}

// Each failure maps to the status a client can act on.
func TestReserveErrorsMapToTheirStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unknown key", store.ErrKeyNotFound, http.StatusUnauthorized},
		// A revoked key is 401 like an unknown one: distinguishing them
		// would confirm which keys exist.
		{"revoked key", store.ErrKeyRevoked, http.StatusUnauthorized},
		{"model not allowed", store.ErrModelNotAllowed, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			api.WriteReserveError(rec, c.err)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}
