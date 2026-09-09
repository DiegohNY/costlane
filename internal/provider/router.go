package provider

import (
	"fmt"
	"strings"
)

// Router maps a model name to the provider that serves it.
//
// The mapping comes from the price table rather than from a hardcoded list,
// so adding a model is a pricing PR and not a code change. An explicit
// "provider/model" prefix overrides it, which is how a model served by two
// providers is disambiguated.
type Router struct {
	providers map[string]Provider
	// modelProvider is rebuilt whenever prices reload.
	modelProvider map[string]string
}

// NewRouter builds a router over the given providers.
func NewRouter(providers ...Provider) *Router {
	byName := make(map[string]Provider, len(providers))
	for _, p := range providers {
		byName[p.Name()] = p
	}
	return &Router{providers: byName, modelProvider: map[string]string{}}
}

// SetModelProviders replaces the model-to-provider mapping.
func (r *Router) SetModelProviders(mapping map[string]string) {
	next := make(map[string]string, len(mapping))
	for model, providerName := range mapping {
		next[model] = providerName
	}
	r.modelProvider = next
}

// ErrNoProvider reports a model no configured provider serves.
type ErrNoProvider struct{ Model string }

func (e *ErrNoProvider) Error() string {
	return fmt.Sprintf("provider: no provider serves model %q", e.Model)
}

// Route resolves a model to its provider, returning the bare model name.
//
// A "provider/model" prefix is honoured first: it is the escape hatch for a
// model two providers both serve, and it is explicit at the call site rather
// than hidden in configuration.
func (r *Router) Route(model string) (Provider, string, error) {
	if name, bare, found := strings.Cut(model, "/"); found {
		p, ok := r.providers[name]
		if !ok {
			return nil, "", &ErrNoProvider{Model: model}
		}
		return p, bare, nil
	}

	name, ok := r.modelProvider[model]
	if !ok {
		return nil, "", &ErrNoProvider{Model: model}
	}
	p, ok := r.providers[name]
	if !ok {
		return nil, "", &ErrNoProvider{Model: model}
	}
	return p, model, nil
}

// Models lists every routable model, for the models endpoint.
func (r *Router) Models() map[string]string {
	out := make(map[string]string, len(r.modelProvider))
	for model, providerName := range r.modelProvider {
		if _, ok := r.providers[providerName]; ok {
			out[model] = providerName
		}
	}
	return out
}
