package api

import (
	"net/http"
)

// handleReconcile reports whether the materialised balances still agree with
// the reservation log.
//
// The three checks are the guard against the one weakness of the budget
// design: a cached balance that drifts from its source. Exposing them means
// an operator can answer "are the numbers right" without reading SQL.
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	report, err := s.db.Reconcile(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, ErrorTypeInternal,
			"the reconciliation could not be run")
		return
	}

	drift := make([]map[string]string, 0, len(report.BalanceDrift))
	for _, d := range report.BalanceDrift {
		drift = append(drift, map[string]string{
			"key_id":                d.KeyID.String(),
			"materialised_spent":    d.MaterialisedSpent.String(),
			"materialised_reserved": d.MaterialisedReserved.String(),
			"log_spent":             d.LogSpent.String(),
			"log_reserved":          d.LogReserved.String(),
		})
	}

	status := http.StatusOK
	if !report.OK() {
		// Not an error — the endpoint worked — but a caller polling this
		// should be able to alert on the status alone.
		status = http.StatusConflict
	}

	WriteJSON(w, status, map[string]any{
		"ok":            report.OK(),
		"balance_drift": drift,
		// Settled reservations with no usage record measure exactly what
		// a crash cost, since the buffer is the only thing between them.
		"settled_without_usage_record": report.SettledWithoutUsageRecord,
		"cost_window_mismatch":         report.CostWindowMismatch,
	})
}

// handlePricingReload re-reads the price files.
//
// The reload is all or nothing: the new set is parsed and validated in full
// before the snapshot is swapped, so a broken file leaves the running table
// serving rather than taking pricing down.
func (s *Server) handlePricingReload(w http.ResponseWriter, r *http.Request) {
	if s.reloadPricing == nil {
		WriteError(w, http.StatusNotImplemented, ErrorTypeInvalidRequest,
			"this build cannot reload prices")
		return
	}
	if err := s.reloadPricing(r.Context()); err != nil {
		// The message carries the parse failure, which is what makes the
		// endpoint useful: an operator sees which file is wrong.
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"the price files were not loaded: "+err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"reloaded": true})
}
