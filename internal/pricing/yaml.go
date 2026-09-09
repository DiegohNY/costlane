package pricing

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

// file is the on-disk shape of one provider's price list.
type file struct {
	Provider string      `yaml:"provider"`
	Models   []yamlModel `yaml:"models"`
	Aliases  []yamlAlias `yaml:"aliases"`
}

type yamlModel struct {
	Model           string      `yaml:"model"`
	SourceURL       string      `yaml:"source_url"`
	FetchedAt       time.Time   `yaml:"fetched_at"`
	ContextWindow   int64       `yaml:"context_window"`
	MaxOutputTokens int64       `yaml:"max_output_tokens"`
	Prices          []yamlPrice `yaml:"prices"`
}

type yamlPrice struct {
	Kind     string `yaml:"kind"`
	Tier     string `yaml:"tier"`
	TierFrom int64  `yaml:"tier_from"`
	TierTo   int64  `yaml:"tier_to"`
	// A string, not a float: the value must survive parsing unchanged, and
	// YAML floats are float64.
	USDPerMTok     string    `yaml:"usd_per_mtok"`
	EffectiveFrom  time.Time `yaml:"effective_from"`
	EffectiveUntil time.Time `yaml:"effective_until"`
	SourceURL      string    `yaml:"source_url"`
}

type yamlAlias struct {
	Alias          string    `yaml:"alias"`
	Canonical      string    `yaml:"canonical_model"`
	EffectiveFrom  time.Time `yaml:"effective_from"`
	EffectiveUntil time.Time `yaml:"effective_until"`
}

// defaultEffectiveFrom applies to rows that do not state one. Rates have to
// start somewhere, and a zero time would sort before every request.
var defaultEffectiveFrom = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// LoadFS reads every *.yaml under dir and builds a validated table.
//
// The whole set is parsed and validated before a table is returned, so a
// single malformed file yields an error and no table at all — never a
// partially loaded one.
func LoadFS(fsys fs.FS, dir string) (*Table, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("pricing: reading %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".yaml" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("pricing: no .yaml files in %s", dir)
	}

	var rows []Row
	var aliases []Alias

	for _, name := range names {
		raw, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("pricing: reading %s: %w", name, err)
		}
		var f file
		// Unknown fields are an error: a typo in a rate key would
		// otherwise silently drop a price.
		dec := yaml.NewDecoder(newReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("pricing: parsing %s: %w", name, err)
		}
		if f.Provider == "" {
			return nil, fmt.Errorf("pricing: %s declares no provider", name)
		}

		fileRows, fileAliases, err := f.convert(name)
		if err != nil {
			return nil, err
		}
		rows = append(rows, fileRows...)
		aliases = append(aliases, fileAliases...)
	}

	return NewTable(rows, aliases)
}

func (f file) convert(name string) ([]Row, []Alias, error) {
	var rows []Row
	for _, m := range f.Models {
		if m.Model == "" {
			return nil, nil, fmt.Errorf("pricing: %s has a model with no name", name)
		}
		if m.SourceURL == "" {
			return nil, nil, fmt.Errorf(
				"pricing: %s/%s has no source_url: a rate nobody can trace is a rate nobody can defend",
				name, m.Model)
		}
		if m.FetchedAt.IsZero() {
			return nil, nil, fmt.Errorf("pricing: %s/%s has no fetched_at", name, m.Model)
		}

		for _, p := range m.Prices {
			amount, err := decimal.NewFromString(p.USDPerMTok)
			if err != nil {
				return nil, nil, fmt.Errorf("pricing: %s/%s %s: bad rate %q: %w",
					name, m.Model, p.Kind, p.USDPerMTok, err)
			}
			tier := Tier(p.Tier)
			if tier == "" {
				tier = TierStandard
			}
			from := p.EffectiveFrom
			if from.IsZero() {
				from = defaultEffectiveFrom
			}
			source := p.SourceURL
			if source == "" {
				source = m.SourceURL
			}

			rows = append(rows, Row{
				Model:      m.Model,
				Provider:   f.Provider,
				Kind:       Kind(p.Kind),
				Tier:       tier,
				USDPerMTok: amount,
				TierFrom:   p.TierFrom,
				TierTo:     p.TierTo,
				From:       from,
				Until:      p.EffectiveUntil,
				SourceURL:  source,
				FetchedAt:  m.FetchedAt,
			})
		}
	}

	var aliases []Alias
	for _, a := range f.Aliases {
		from := a.EffectiveFrom
		if from.IsZero() {
			from = defaultEffectiveFrom
		}
		aliases = append(aliases, Alias{
			Alias:     a.Alias,
			Canonical: a.Canonical,
			Provider:  f.Provider,
			From:      from,
			Until:     a.EffectiveUntil,
		})
	}
	return rows, aliases, nil
}
