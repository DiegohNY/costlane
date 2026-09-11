package provider_test

import (
	"testing"

	"github.com/DiegohNY/costlane/internal/provider"
)

// The thinking table is data with provenance, in the shape the price seed
// uses: every level was read from the page named beside it, on the date
// named beside it. A level nobody can trace is a level nobody can defend,
// and thinking is billed as output.
func TestGoogleThinkingTableIsSourced(t *testing.T) {
	table, err := provider.GoogleThinkingTable()
	if err != nil {
		t.Fatalf("loading the thinking seed: %v", err)
	}
	if len(table) == 0 {
		t.Fatal("the thinking seed declares no models")
	}

	for _, model := range table {
		if model.SourceURL == "" || model.FetchedAt.IsZero() {
			t.Errorf("%s has no source_url or no fetched_at", model.Model)
		}
		if len(model.Levels) == 0 {
			t.Errorf("%s declares no levels", model.Model)
		}
		for _, level := range model.Levels {
			if level.SourceURL == "" {
				t.Errorf("%s/%s has no source_url", model.Model, level.ReasoningEffort)
			}
		}
	}
}

// What the documentation says, model by model. Each expectation here is a
// line in the seed, and each line in the seed is a line on a Google page.
func TestGoogleThinkingLevelMapping(t *testing.T) {
	cases := []struct {
		model  string
		effort string
		want   string
		mapped bool
		why    string
	}{
		// gemini-3.8-flash supports low, medium and high.
		{"gemini-3.8-flash", "low", "low", true, ""},
		{"gemini-3.8-flash", "medium", "medium", true, ""},
		{"gemini-3.8-flash", "high", "high", true, ""},
		// "minimal thinking level is not supported for Gemini 3.8 Flash
		// and will return an error", and Google publishes no substitute
		// for this model, so costlane refuses rather than inventing one.
		{"gemini-3.8-flash", "minimal", "", false,
			"minimal is an error on this model and Google documents no substitute"},
		// "Reasoning cannot be turned off for Gemini 2.5 Pro or 3 models."
		{"gemini-3.8-flash", "none", "", false,
			"thinking cannot be disabled on a Gemini 3 model"},

		// gemini-3.1-pro-preview supports low, medium and high, and
		// Google's own reasoning_effort table maps minimal onto low.
		{"gemini-3.1-pro-preview", "minimal", "low", true, ""},
		{"gemini-3.1-pro-preview", "low", "low", true, ""},
		{"gemini-3.1-pro-preview", "medium", "medium", true, ""},
		{"gemini-3.1-pro-preview", "high", "high", true, ""},
		{"gemini-3.1-pro-preview", "none", "", false,
			"thinking cannot be disabled on a Gemini 3 model"},

		// A model with no table at all maps nothing. Fail closed: a
		// caller asking for less thinking on a model costlane cannot
		// configure must not be answered as though it had been applied.
		{"gemini-9-unreleased", "low", "", false, "no table for this model"},
	}

	for _, c := range cases {
		t.Run(c.model+"/"+c.effort, func(t *testing.T) {
			got, ok := provider.GoogleThinkingLevel(c.model, c.effort)
			if ok != c.mapped {
				t.Fatalf("mapped = %v, want %v (%s)", ok, c.mapped, c.why)
			}
			if got != c.want {
				t.Errorf("level = %q, want %q", got, c.want)
			}
		})
	}
}
