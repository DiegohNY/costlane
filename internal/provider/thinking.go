package provider

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed all:seed
var thinkingSeedFS embed.FS

// ThinkingModel is one model's mapping from reasoning_effort to Gemini's
// thinkingLevel, with the provenance the seed carries.
type ThinkingModel struct {
	Provider  string
	Model     string
	SourceURL string
	FetchedAt time.Time
	Levels    []ThinkingLevel
}

// ThinkingLevel is one mapped level and the page it was read from.
//
// ProviderLevel is the destination's own spelling: Gemini's thinkingLevel,
// Anthropic's output_config.effort. The same OpenAI word means a different
// field in each dialect, which is why the mapping is per provider and per
// model rather than one table.
type ThinkingLevel struct {
	ReasoningEffort string
	ProviderLevel   string
	SourceURL       string
}

// reasoningEfforts is the vocabulary an incoming request may use. A value
// outside it is refused by name rather than mapped to something nearby: a
// caller who mistypes a level must not be quietly given a different one and
// billed for the difference.
var reasoningEfforts = map[string]bool{
	"none": true, "minimal": true, "low": true, "medium": true, "high": true,
}

type thinkingFile struct {
	Provider string              `yaml:"provider"`
	Models   []thinkingYAMLModel `yaml:"models"`
}

type thinkingYAMLModel struct {
	Model     string              `yaml:"model"`
	SourceURL string              `yaml:"source_url"`
	FetchedAt time.Time           `yaml:"fetched_at"`
	Levels    []thinkingYAMLLevel `yaml:"levels"`
}

type thinkingYAMLLevel struct {
	ReasoningEffort string `yaml:"reasoning_effort"`
	ProviderLevel   string `yaml:"provider_level"`
	SourceURL       string `yaml:"source_url"`
}

// googleThinking parses the seed once. A failure is kept rather than
// panicked on: the table is consulted only when a request asks for a
// thinking level, and a table that did not load maps nothing — which refuses
// those requests instead of sending them upstream without the ceiling the
// caller asked for.
var thinkingSeed = sync.OnceValues(func() ([]ThinkingModel, error) {
	entries, err := fs.ReadDir(thinkingSeedFS, "seed")
	if err != nil {
		return nil, fmt.Errorf("provider: reading the thinking seed: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".yaml" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("provider: no .yaml files in the thinking seed")
	}

	var models []ThinkingModel
	for _, name := range names {
		fileModels, err := loadThinkingFile(name)
		if err != nil {
			return nil, err
		}
		models = append(models, fileModels...)
	}
	return models, nil
})

// loadThinkingFile reads one provider's levels. The whole set is parsed
// before any of it is used, so a malformed file yields no table at all
// rather than a partial one that maps some models and silently not others.
func loadThinkingFile(name string) ([]ThinkingModel, error) {
	raw, err := thinkingSeedFS.ReadFile(path.Join("seed", name))
	if err != nil {
		return nil, fmt.Errorf("provider: reading %s: %w", name, err)
	}

	var f thinkingFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// Unknown fields are an error: a typo in a key would otherwise drop a
	// level silently, and a dropped level is a level billed at the
	// model's own default.
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("provider: parsing %s: %w", name, err)
	}
	if f.Provider == "" {
		return nil, fmt.Errorf("provider: %s declares no provider", name)
	}

	var models []ThinkingModel
	for _, m := range f.Models {
		switch {
		case m.Model == "":
			return nil, fmt.Errorf("provider: thinking seed has a model with no name")
		case m.SourceURL == "":
			return nil, fmt.Errorf(
				"provider: thinking seed %s has no source_url: a level nobody can trace is a level nobody can defend",
				m.Model)
		case m.FetchedAt.IsZero():
			return nil, fmt.Errorf("provider: thinking seed %s has no fetched_at", m.Model)
		case len(m.Levels) == 0:
			return nil, fmt.Errorf("provider: thinking seed %s declares no levels", m.Model)
		}

		out := ThinkingModel{
			Provider:  f.Provider,
			Model:     m.Model,
			SourceURL: m.SourceURL,
			FetchedAt: m.FetchedAt,
		}
		for _, l := range m.Levels {
			if !reasoningEfforts[l.ReasoningEffort] {
				return nil, fmt.Errorf("provider: thinking seed %s: %q is not a reasoning_effort",
					m.Model, l.ReasoningEffort)
			}
			if l.ProviderLevel == "" {
				return nil, fmt.Errorf("provider: thinking seed %s/%s has no provider_level",
					m.Model, l.ReasoningEffort)
			}
			source := l.SourceURL
			if source == "" {
				source = m.SourceURL
			}
			out.Levels = append(out.Levels, ThinkingLevel{
				ReasoningEffort: l.ReasoningEffort,
				ProviderLevel:   l.ProviderLevel,
				SourceURL:       source,
			})
		}
		models = append(models, out)
	}
	return models, nil
}

// GoogleThinkingTable returns the seeded Gemini mapping, with its
// provenance.
func GoogleThinkingTable() ([]ThinkingModel, error) { return thinkingTableFor("google") }

// AnthropicEffortTable returns the seeded Anthropic mapping.
func AnthropicEffortTable() ([]ThinkingModel, error) { return thinkingTableFor("anthropic") }

func thinkingTableFor(providerName string) ([]ThinkingModel, error) {
	all, err := thinkingSeed()
	if err != nil {
		return nil, err
	}
	var out []ThinkingModel
	for _, m := range all {
		if m.Provider == providerName {
			out = append(out, m)
		}
	}
	return out, nil
}

// GoogleThinkingLevel resolves a reasoning_effort to Gemini's thinkingLevel
// for one model.
//
// It fails closed in three directions, all of which end in a refusal rather
// than in a request sent upstream unconfigured: a model with no table, a
// level with no row, and a seed that did not load.
func GoogleThinkingLevel(model, effort string) (string, bool) {
	return mappedLevel("google", model, effort)
}

// AnthropicEffort resolves a reasoning_effort to Anthropic's
// output_config.effort for one model, failing closed in the same three
// directions.
func AnthropicEffort(model, effort string) (string, bool) {
	return mappedLevel("anthropic", model, effort)
}

func mappedLevel(providerName, model, effort string) (string, bool) {
	table, err := thinkingSeed()
	if err != nil {
		return "", false
	}
	for _, m := range table {
		if m.Provider != providerName || m.Model != model {
			continue
		}
		for _, l := range m.Levels {
			if l.ReasoningEffort == effort {
				return l.ProviderLevel, true
			}
		}
		return "", false
	}
	return "", false
}

// googleThinkingRefusal explains a refusal in terms a caller can act on: the
// level they asked for, and the levels this model actually has. A refusal
// that says only "unsupported" sends them to read our source.
func thinkingRefusal(providerName, model, effort string) string {
	if !reasoningEfforts[effort] {
		return fmt.Sprintf(
			"%q is not a reasoning_effort level; costlane accepts none, minimal, low, medium and high",
			effort)
	}

	table, err := thinkingSeed()
	if err != nil {
		return fmt.Sprintf(
			"costlane could not load its thinking table, so %q cannot be mapped for %s",
			effort, model)
	}

	for _, m := range table {
		if m.Provider != providerName || m.Model != model {
			continue
		}
		var mapped []string
		for _, l := range m.Levels {
			mapped = append(mapped, l.ReasoningEffort)
		}
		return fmt.Sprintf(
			"%s has no documented thinking level for %q; costlane maps %s for this model, "+
				"and a level Google does not publish is refused rather than guessed at",
			model, effort, strings.Join(mapped, ", "))
	}

	return fmt.Sprintf(
		"costlane has no sourced thinking table for %s, so %q cannot be applied; "+
			"forwarding the request without it would bill the caller for the thinking they asked to limit",
		model, effort)
}
