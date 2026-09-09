package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/DiegohNY/costlane/internal/store"
)

// defaultUsageWindow is how far back a query reaches when no range is given.
const defaultUsageWindow = 24 * time.Hour

// resolveScope decides whose data a caller may read.
//
// A master credential sees everything; a virtual key sees only itself. The
// scope is then carried into the repository, which requires it — a handler
// that forgot to filter would not compile.
func (s *Server) resolveScope(r *http.Request) (auth.Scope, bool) {
	token, ok := BearerToken(r)
	if !ok {
		return auth.Scope{}, false
	}
	if auth.VerifyMaster(s.masterKey, token) {
		return auth.MasterScope(), true
	}
	key, err := s.db.FindKeyByHash(r.Context(), auth.Hash(token))
	if err != nil || key.RevokedAt != nil {
		return auth.Scope{}, false
	}
	return auth.KeyScope(key.ID), true
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.resolveScope(r)
	if !ok {
		WriteError(w, http.StatusUnauthorized, ErrorTypeInvalidAPIKey,
			"a valid credential is required")
		return
	}

	from, to, err := timeRange(r)
	if err != nil {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, err.Error())
		return
	}

	var groupBy []store.Dimension
	if raw := r.URL.Query().Get("group_by"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			groupBy = append(groupBy, store.Dimension(strings.TrimSpace(part)))
		}
	}

	buckets, err := s.db.QueryUsage(r.Context(), store.UsageQuery{
		Scope: scope, From: from, To: to, GroupBy: groupBy,
		MaxWindow: s.maxQueryWindow,
	})
	if err != nil {
		writeQueryError(w, err)
		return
	}
	if buckets == nil {
		buckets = []store.UsageBucket{}
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"data": buckets,
		"from": from.Format(time.RFC3339),
		"to":   to.Format(time.RFC3339),
	})
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.resolveScope(r)
	if !ok {
		WriteError(w, http.StatusUnauthorized, ErrorTypeInvalidAPIKey,
			"a valid credential is required")
		return
	}

	from, to, err := timeRange(r)
	if err != nil {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, err.Error())
		return
	}

	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
				"limit must be a positive integer")
			return
		}
		limit = parsed
	}

	page, err := s.db.QueryRequests(r.Context(), store.RequestQuery{
		Scope: scope, From: from, To: to, Limit: limit,
		Cursor: r.URL.Query().Get("cursor"), MaxWindow: s.maxQueryWindow,
	})
	if err != nil {
		writeQueryError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

func (s *Server) handleBudget(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.resolveScope(r)
	if !ok {
		WriteError(w, http.StatusUnauthorized, ErrorTypeInvalidAPIKey,
			"a valid credential is required")
		return
	}
	if scope.IsMaster() {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"this endpoint reports the calling key's own budget; "+
				"a master credential has none")
		return
	}

	status, err := s.db.BudgetStatusFor(r.Context(), scope.KeyID())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, ErrorTypeInternal,
			"the budget could not be read")
		return
	}
	WriteJSON(w, http.StatusOK, status)
}

// timeRange reads the window, defaulting to the last day.
func timeRange(r *http.Request) (from, to time.Time, err error) {
	query := r.URL.Query()
	to = time.Now().UTC()
	from = to.Add(-defaultUsageWindow)

	if raw := query.Get("from"); raw != "" {
		from, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{},
				errors.New("from must be an RFC3339 timestamp, for example 2026-09-01T00:00:00Z")
		}
	}
	if raw := query.Get("to"); raw != "" {
		to, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{},
				errors.New("to must be an RFC3339 timestamp, for example 2026-09-30T00:00:00Z")
		}
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, errors.New("to must be after from")
	}
	return from.UTC(), to.UTC(), nil
}

// writeQueryError maps a refused query to a status a caller can act on.
//
// A malformed cursor is the caller's mistake, not ours: reporting 500 would
// send someone looking for a fault in the gateway.
func writeQueryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrTooManyDimensions):
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"at most two group_by dimensions may be combined")
	case errors.Is(err, store.ErrUnknownDimension):
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"group_by accepts key, model, provider, day or hour")
	case errors.Is(err, store.ErrRangeTooWide):
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"the requested range is wider than this gateway allows")
	case errors.Is(err, store.ErrBadCursor):
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"the cursor is not one this endpoint issued")
	default:
		WriteError(w, http.StatusInternalServerError, ErrorTypeInternal,
			"the query could not be completed")
	}
}
