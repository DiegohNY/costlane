package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/config"
	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/provider"
)

// buildRouter is reached only from this command, and until v0.2.1 nothing
// crossed it: the adapters, the streaming tests and the live verification all
// construct a provider directly, and the one end-to-end test that goes through
// here asked for an OpenAI model.
//
// So Google was never registered. Configuration read the key, the redactor
// scrubbed it, the README documented it, and the price seed mapped every
// Gemini model to a provider named "google" that did not exist in the router —
// which made every Gemini request a 404 from the binary, through two releases.
//
// These tests are the seam's own.

func testTable(t *testing.T) *pricing.Table {
	t.Helper()
	table, err := pricing.LoadSeed()
	if err != nil {
		t.Fatalf("loading prices: %v", err)
	}
	return table
}

// aModelFor returns a model the seed says this provider serves, so the test
// does not hard-code a name the price list is free to change.
func aModelFor(t *testing.T, table *pricing.Table, providerName string) string {
	t.Helper()
	for model, owner := range table.ModelProviders() {
		if owner == providerName {
			return model
		}
	}
	t.Fatalf("the price seed lists no model for provider %q", providerName)
	return ""
}

// With every credential set, every provider is routable.
func TestBuildRouterRegistersEveryConfiguredProvider(t *testing.T) {
	table := testTable(t)
	cfg := &config.Config{
		OpenAIKey:    obs.Secret("test-openai"),
		AnthropicKey: obs.Secret("test-anthropic"),
		GoogleKey:    obs.Secret("test-google"),
	}

	router, err := buildRouter(cfg, &http.Client{Timeout: time.Second}, table)
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}

	for _, name := range []string{"openai", "anthropic", "google"} {
		model := aModelFor(t, table, name)
		p, bare, err := router.Route(model)
		if err != nil {
			t.Errorf("Route(%q) failed for provider %q: %v. The price seed maps "+
				"that model to %q, so a router without it answers 404 for every "+
				"model that provider serves.", model, name, err, name)
			continue
		}
		if p.Name() != name {
			t.Errorf("Route(%q) resolved to provider %q, want %q", model, p.Name(), name)
		}
		if bare != model {
			t.Errorf("Route(%q) returned bare model %q", model, bare)
		}
	}
}

// With one credential, the others are not routable and are not advertised.
//
// The second half is what would have caught the original bug at boot without
// calling anybody: /v1/models is built from Router.Models, which lists only
// models whose provider is actually registered.
func TestBuildRouterOmitsProvidersWithoutACredential(t *testing.T) {
	table := testTable(t)
	cfg := &config.Config{OpenAIKey: obs.Secret("test-openai")}

	router, err := buildRouter(cfg, &http.Client{Timeout: time.Second}, table)
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}

	// The configured one works.
	openaiModel := aModelFor(t, table, "openai")
	if _, _, err := router.Route(openaiModel); err != nil {
		t.Errorf("Route(%q) failed for the one configured provider: %v",
			openaiModel, err)
	}

	// The others are refused, and refused by the right error.
	for _, name := range []string{"anthropic", "google"} {
		model := aModelFor(t, table, name)
		if _, _, err := router.Route(model); err == nil {
			t.Errorf("Route(%q) succeeded with no %s credential configured",
				model, name)
		} else {
			var missing *provider.ErrNoProvider
			if !errors.As(err, &missing) {
				t.Errorf("Route(%q) failed with %v, want ErrNoProvider", model, err)
			}
		}
	}

	// And they are not offered to a client either. Listing a model that
	// cannot be routed invites a 404 that looks like our fault.
	listed := router.Models()
	for _, name := range []string{"anthropic", "google"} {
		for model, owner := range listed {
			if owner == name {
				t.Errorf("/v1/models would list %q, owned by %q, with no %s "+
					"credential configured", model, owner, name)
			}
		}
	}
	if len(listed) == 0 {
		t.Error("no models listed at all, though OpenAI is configured")
	}
}

// No credential at all is a refusal to boot, and the message names every
// variable that would fix it. The original named two of the three, which is
// how a Google-only deployment was told its own configuration did not exist.
func TestBuildRouterRefusesWithNoCredentialAndNamesAllThree(t *testing.T) {
	_, err := buildRouter(&config.Config{}, &http.Client{}, testTable(t))
	if err == nil {
		t.Fatal("building a router with no provider credential must fail")
	}
	for _, name := range []string{
		"COSTLANE_OPENAI_API_KEY",
		"COSTLANE_ANTHROPIC_API_KEY",
		"COSTLANE_GOOGLE_API_KEY",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not mention %s: %v", name, err)
		}
	}
}
