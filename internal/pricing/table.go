// Package pricing resolves model aliases and computes request cost.
//
// Every monetary value here is a decimal. Rates are fractions of a cent per
// token, and float64 cannot represent them exactly: a gateway accumulating
// millions of such values drifts away from the provider's own invoice, which
// is the one number this product exists to match.
//
// Prices are stored long — one row per (model, provider, kind, tier, context
// range, validity window) — because providers keep inventing billing classes.
// A wide table would need a migration each time; here a new class is a row.
package pricing

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

var perMillion = decimal.NewFromInt(1_000_000)

// ErrNoRows reports that the table holds nothing for a model at all.
var ErrNoRows = errors.New("pricing: no rows for this model")

// Counts is the per-kind token count of one request. It is the sole input to
// the cost calculation; the aggregate columns on a usage record are derived
// from it for reporting and never used to compute money.
type Counts map[Kind]int64

// Row is one published rate.
type Row struct {
	ID       string
	Model    string
	Provider string
	Kind     Kind
	Tier     Tier

	USDPerMTok decimal.Decimal

	// TierFrom and TierTo bound the context range as [from, to), matching
	// the database exclusion constraint. A zero TierTo means unbounded.
	TierFrom int64
	TierTo   int64

	// From is inclusive, Until exclusive. A zero Until means open-ended.
	From  time.Time
	Until time.Time

	SourceURL string
	FetchedAt time.Time
}

// Covers reports whether this row applies at the given instant and input size.
func (r Row) Covers(at time.Time, inputTokens int64) bool {
	if at.Before(r.From) {
		return false
	}
	if !r.Until.IsZero() && !at.Before(r.Until) {
		return false
	}
	if inputTokens < r.TierFrom {
		return false
	}
	return r.TierTo == 0 || inputTokens < r.TierTo
}

// Alias maps an undated model name to the snapshot it resolved to during a
// given period.
//
// Note that not every undated name is an alias: from the Claude 4.6
// generation on, a dateless id is itself the pinned snapshot. Only names a
// provider documents as aliases belong here.
type Alias struct {
	Alias     string
	Canonical string
	Provider  string
	From      time.Time
	Until     time.Time
}

// Covers reports whether this mapping was in force at the given instant.
func (a Alias) Covers(at time.Time) bool {
	if at.Before(a.From) {
		return false
	}
	return a.Until.IsZero() || at.Before(a.Until)
}

// Request is what to price.
type Request struct {
	Model    string
	Provider string
	Tier     Tier
	At       time.Time
	Counts   Counts
}

// Result is the outcome of pricing one request.
//
// Unpriced and PartiallyPriced are different states. Unpriced means the table
// knows nothing about the model, so the cost is unknown — never zero.
// PartiallyPriced means some kinds were priced and others were not: the
// request spent real money, so charging zero would be the largest budget hole
// in the system. Charging the known part and saying so is the only
// fail-closed option.
type Result struct {
	Cost            decimal.Decimal
	Unpriced        bool
	PartiallyPriced bool
	UnpricedKinds   []Kind
	// PricedKinds records which kinds contributed, so a settle can explain
	// its own figure.
	PricedKinds []Kind
}

// Table is an immutable snapshot of the price list.
//
// It is replaced wholesale on reload rather than mutated, so a request either
// sees the old table or the new one, never a half-applied mixture.
type Table struct {
	rows    map[modelKey][]Row
	aliases map[aliasKey][]Alias
}

type modelKey struct{ model, provider string }
type aliasKey struct{ alias, provider string }

// NewTable validates the rows and builds a snapshot. Validation happens here,
// before any swap, so a broken price file cannot replace a working table.
func NewTable(rows []Row, aliases []Alias) (*Table, error) {
	t := &Table{
		rows:    make(map[modelKey][]Row),
		aliases: make(map[aliasKey][]Alias),
	}

	for i, r := range rows {
		if err := validateRow(r); err != nil {
			return nil, fmt.Errorf("pricing: row %d (%s/%s %s): %w",
				i, r.Provider, r.Model, r.Kind, err)
		}
		k := modelKey{r.Model, r.Provider}
		t.rows[k] = append(t.rows[k], r)
	}

	for i, a := range aliases {
		if err := validateAlias(a); err != nil {
			return nil, fmt.Errorf("pricing: alias %d (%s): %w", i, a.Alias, err)
		}
		k := aliasKey{a.Alias, a.Provider}
		t.aliases[k] = append(t.aliases[k], a)
	}

	// An alias pointing at a model with no prices resolves to nothing
	// useful, and is a mistake worth catching at load rather than at the
	// first request that hits it.
	for k, as := range t.aliases {
		for _, a := range as {
			if _, ok := t.rows[modelKey{a.Canonical, a.Provider}]; !ok {
				return nil, fmt.Errorf(
					"pricing: alias %q resolves to %q, which has no prices",
					k.alias, a.Canonical)
			}
		}
	}

	if err := t.checkOverlaps(); err != nil {
		return nil, err
	}
	return t, nil
}

// Resolve maps a possibly-aliased model name to its canonical form at an
// instant. A name with no alias row is already canonical, which is the
// ordinary case for providers whose dateless ids are pinned snapshots.
func (t *Table) Resolve(model, provider string, at time.Time) string {
	for _, a := range t.aliases[aliasKey{model, provider}] {
		if a.Covers(at) {
			return a.Canonical
		}
	}
	return model
}

// Cost prices a request.
//
// The context tier is selected once, from the total of every kind that counts
// towards input, and then applied to every kind including output. OpenAI's
// long-context pricing works this way: crossing the threshold reprices the
// whole request rather than only the tokens beyond it.
func (t *Table) Cost(req Request) (Result, error) {
	canonical := t.Resolve(req.Model, req.Provider, req.At)
	rows, ok := t.rows[modelKey{canonical, req.Provider}]
	if !ok {
		return Result{Unpriced: true}, nil
	}

	inputTotal := req.Counts.InputTierTotal()

	total := decimal.Zero
	var priced, unpriced []Kind

	for _, kind := range sortedKinds(req.Counts) {
		count := req.Counts[kind]
		if count == 0 {
			continue
		}
		// A breakdown of a kind that was already charged. Pricing it here
		// would bill the same tokens twice; treating its missing rate as
		// unpriced would flag a cost that is in fact complete.
		if !kind.Billable() {
			continue
		}
		if count < 0 {
			return Result{}, fmt.Errorf("pricing: %s count is negative (%d)", kind, count)
		}

		row, found := pick(rows, kind, req.Tier, req.At, inputTotal)
		if !found {
			unpriced = append(unpriced, kind)
			continue
		}
		total = total.Add(decimal.NewFromInt(count).Mul(row.USDPerMTok).Div(perMillion))
		priced = append(priced, kind)
	}

	// Nothing at all could be priced: report unknown rather than a zero
	// that would read as free.
	if len(priced) == 0 && len(unpriced) > 0 {
		return Result{Unpriced: true, UnpricedKinds: unpriced}, nil
	}

	return Result{
		Cost:            total,
		PartiallyPriced: len(unpriced) > 0,
		UnpricedKinds:   unpriced,
		PricedKinds:     priced,
	}, nil
}

// InputTierTotal is the token count that selects a context tier. Cached and
// cache-write tokens count: they are part of what the model reads, and
// OpenAI measures its threshold on input tokens.
func (c Counts) InputTierTotal() int64 {
	var total int64
	for kind, n := range c {
		if n > 0 && kind.CountsTowardInputTier() {
			total += n
		}
	}
	return total
}

func pick(rows []Row, kind Kind, tier Tier, at time.Time, inputTokens int64) (Row, bool) {
	for _, r := range rows {
		if r.Kind == kind && r.Tier == tier && r.Covers(at, inputTokens) {
			return r, true
		}
	}
	return Row{}, false
}

// checkOverlaps mirrors the database exclusion constraint in memory, so a
// bad file is rejected before it reaches Postgres and with a clearer message.
func (t *Table) checkOverlaps() error {
	for k, rows := range t.rows {
		for i := range rows {
			for j := i + 1; j < len(rows); j++ {
				a, b := rows[i], rows[j]
				if a.Kind != b.Kind || a.Tier != b.Tier {
					continue
				}
				if rangesOverlap(a.TierFrom, a.TierTo, b.TierFrom, b.TierTo) &&
					periodsOverlap(a.From, a.Until, b.From, b.Until) {
					return fmt.Errorf(
						"pricing: %s/%s has overlapping rows for %s on tier %s",
						k.provider, k.model, a.Kind, a.Tier)
				}
			}
		}
	}
	for k, as := range t.aliases {
		for i := range as {
			for j := i + 1; j < len(as); j++ {
				if periodsOverlap(as[i].From, as[i].Until, as[j].From, as[j].Until) {
					return fmt.Errorf("pricing: alias %q has overlapping mappings", k.alias)
				}
			}
		}
	}
	return nil
}

func rangesOverlap(aFrom, aTo, bFrom, bTo int64) bool {
	if aTo == 0 {
		aTo = int64(^uint64(0) >> 1)
	}
	if bTo == 0 {
		bTo = int64(^uint64(0) >> 1)
	}
	return aFrom < bTo && bFrom < aTo
}

func periodsOverlap(aFrom, aUntil, bFrom, bUntil time.Time) bool {
	if !aUntil.IsZero() && !aUntil.After(bFrom) {
		return false
	}
	if !bUntil.IsZero() && !bUntil.After(aFrom) {
		return false
	}
	return true
}

func validateRow(r Row) error {
	if r.Model == "" || r.Provider == "" {
		return errors.New("model and provider are required")
	}
	if !r.Kind.Valid() {
		return fmt.Errorf("unknown token kind %q", r.Kind)
	}
	if !r.Tier.Valid() {
		return fmt.Errorf("unknown service tier %q", r.Tier)
	}
	if r.USDPerMTok.IsNegative() {
		return fmt.Errorf("negative rate %s", r.USDPerMTok)
	}
	if r.From.IsZero() {
		return errors.New("effective_from is required")
	}
	if !r.Until.IsZero() && !r.Until.After(r.From) {
		return errors.New("effective_until must be after effective_from")
	}
	if r.TierFrom < 0 || (r.TierTo != 0 && r.TierTo <= r.TierFrom) {
		return errors.New("invalid context tier bounds")
	}
	if r.SourceURL == "" {
		return errors.New("source_url is required: a rate nobody can trace is a rate nobody can defend")
	}
	return nil
}

func validateAlias(a Alias) error {
	if a.Alias == "" || a.Canonical == "" || a.Provider == "" {
		return errors.New("alias, canonical model and provider are required")
	}
	if a.From.IsZero() {
		return errors.New("effective_from is required")
	}
	if !a.Until.IsZero() && !a.Until.After(a.From) {
		return errors.New("effective_until must be after effective_from")
	}
	return nil
}

func sortedKinds(c Counts) []Kind {
	out := make([]Kind, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// decimalFromString parses a decimal, used by tests to normalise literals.
func decimalFromString(s string) (decimal.Decimal, error) {
	return decimal.NewFromString(s)
}

// Rows returns every row in the table, for tooling that audits the seed.
func (t *Table) Rows() []Row {
	var out []Row
	for _, rows := range t.rows {
		out = append(out, rows...)
	}
	return out
}

// HasActivePrice reports whether the table can price this model now.
//
// A model with no active price cannot be metered, which is the one thing this
// gateway exists to do, so it is not offered to clients.
func (t *Table) HasActivePrice(model, provider string, at time.Time) bool {
	for _, r := range t.rows[modelKey{model, provider}] {
		if r.Covers(at, 0) {
			return true
		}
	}
	return false
}

// ModelProviders maps every priced model to the provider that serves it.
//
// Routing follows pricing rather than a separate list, so a model cannot be
// routable without being priceable — which is the state that would let
// untracked spend through.
func (t *Table) ModelProviders() map[string]string {
	out := make(map[string]string, len(t.rows))
	for key := range t.rows {
		out[key.model] = key.provider
	}
	for key, aliases := range t.aliases {
		for _, a := range aliases {
			out[key.alias] = a.Provider
		}
	}
	return out
}
