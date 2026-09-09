// Command pricingcheck verifies that every seeded rate still appears on the
// page it was read from.
//
// It is deliberately not part of the blocking CI: it depends on external
// pages that can restyle, rate-limit, or go down for reasons that have
// nothing to do with this repository. It reports, and a human decides.
//
// The distinction it works hardest to preserve is between "this rate
// changed" and "I could not check". Reporting an unverifiable rate as
// verified would be worse than not running at all.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/DiegohNY/costlane/internal/pricing"
)

func main() {
	if err := run(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

type finding struct {
	model, provider, source string
	kind                    pricing.Kind
	rate                    string
	status                  string
}

const (
	statusFound       = "found"
	statusMissing     = "MISSING"
	statusUnreachable = "UNREACHABLE"
)

func run() error {
	table, err := pricing.LoadSeed()
	if err != nil {
		return fmt.Errorf("the seed does not load: %w", err)
	}

	pages := map[string]pageResult{}
	var findings []finding

	for _, row := range table.Rows() {
		page, ok := pages[row.SourceURL]
		if !ok {
			page = fetch(row.SourceURL)
			pages[row.SourceURL] = page
		}

		f := finding{
			model: row.Model, provider: row.Provider, kind: row.Kind,
			rate: row.USDPerMTok.String(), source: row.SourceURL,
		}
		switch {
		case page.err != nil:
			f.status = statusUnreachable
		case containsRate(page.body, row.USDPerMTok.String()):
			f.status = statusFound
		default:
			f.status = statusMissing
		}
		findings = append(findings, f)
	}

	return report(findings, pages)
}

type pageResult struct {
	body string
	err  error
}

func fetch(url string) pageResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return pageResult{err: err}
	}
	req.Header.Set("User-Agent", "costlane-pricing-check/1.0 (+https://github.com/DiegohNY/costlane)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pageResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return pageResult{err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return pageResult{err: err}
	}
	return pageResult{body: string(body)}
}

// containsRate reports whether the page states this rate anywhere.
//
// It compares values rather than characters: a page printing "$12.50" and a
// table holding "12.5" mean the same thing, while "10" and "100" do not. A
// plain substring search gets both of those wrong in the dangerous
// direction, reporting a rate that has vanished as still present.
func containsRate(body, rate string) bool {
	want, err := decimal.NewFromString(rate)
	if err != nil {
		return false
	}
	for _, n := range numbersIn(body) {
		if n.Equal(want) {
			return true
		}
	}
	return false
}

// numbersIn extracts every decimal literal in the text. Grouping commas are
// dropped so that "1,050,000" reads as one number, and a trailing period is
// treated as punctuation rather than as part of the figure.
func numbersIn(body string) []decimal.Decimal {
	var out []decimal.Decimal
	var buf strings.Builder

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		text := strings.TrimSuffix(buf.String(), ".")
		buf.Reset()
		if text == "" || text == "." {
			return
		}
		if d, err := decimal.NewFromString(text); err == nil {
			out = append(out, d)
		}
	}

	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= '0' && c <= '9':
			buf.WriteByte(c)
		case c == '.' && buf.Len() > 0:
			buf.WriteByte(c)
		case c == ',' && buf.Len() > 0 && i+1 < len(body) && isDigit(body[i+1]):
			// A grouping separator inside a number.
		default:
			flush()
		}
	}
	flush()
	return out
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func report(findings []finding, pages map[string]pageResult) error {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].provider != findings[j].provider {
			return findings[i].provider < findings[j].provider
		}
		if findings[i].model != findings[j].model {
			return findings[i].model < findings[j].model
		}
		return findings[i].kind < findings[j].kind
	})

	var missing, unreachable []finding
	for _, f := range findings {
		switch f.status {
		case statusMissing:
			missing = append(missing, f)
		case statusUnreachable:
			unreachable = append(unreachable, f)
		}
	}

	fmt.Printf("Checked %d seeded rates across %d pages.\n\n", len(findings), len(pages))

	if len(unreachable) > 0 {
		fmt.Println("## Could not be checked")
		fmt.Println()
		fmt.Println("These pages did not load, so their rates are neither confirmed nor")
		fmt.Println("refuted. They need a manual look.")
		fmt.Println()
		seen := map[string]bool{}
		for _, f := range unreachable {
			if seen[f.source] {
				continue
			}
			seen[f.source] = true
			fmt.Printf("- %s — %v\n", f.source, pages[f.source].err)
		}
		fmt.Println()
	}

	if len(missing) > 0 {
		fmt.Println("## No longer found on the page")
		fmt.Println()
		fmt.Println("Each rate below is seeded but no longer appears on its source page.")
		fmt.Println("Either the provider repriced, or the page changed shape.")
		fmt.Println()
		for _, f := range missing {
			fmt.Printf("- `%s/%s` %s = %s — %s\n", f.provider, f.model, f.kind, f.rate, f.source)
		}
		fmt.Println()
	}

	if len(missing) == 0 && len(unreachable) == 0 {
		fmt.Println("Every seeded rate still appears on its source page.")
		return nil
	}
	return fmt.Errorf("%d rates missing, %d unreachable", len(missing), len(unreachable))
}
