package proxy

import (
	"net/http"
	"sort"
	"time"

	"github.com/DiegohNY/costlane/internal/api"
)

// ModelsHandler serves GET /v1/models.
//
// It answers from the local price table rather than by asking the providers.
// Many client libraries call this at startup, and an upstream round trip
// there would make the gateway's own availability depend on three others.
type ModelsHandler struct {
	opts Options
}

// NewModelsHandler builds the models endpoint.
func NewModelsHandler(opts Options) *ModelsHandler { return &ModelsHandler{opts: opts} }

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ServeHTTP lists the models this gateway can price and route.
func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.BearerToken(r); !ok {
		api.WriteError(w, http.StatusUnauthorized, api.ErrorTypeInvalidAPIKey,
			"a credential is required in the Authorization header")
		return
	}

	routable := h.opts.Router.Models()
	table := h.opts.Pricing.Table()
	now := time.Now().UTC()

	entries := make([]modelEntry, 0, len(routable))
	for model, providerName := range routable {
		// A model nobody can price is a model this gateway cannot meter,
		// so it is not offered.
		if !table.HasActivePrice(model, providerName, now) {
			continue
		}
		entries = append(entries, modelEntry{
			ID: model, Object: "model", Created: 0, OwnedBy: providerName,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	api.WriteJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   entries,
	})
}
