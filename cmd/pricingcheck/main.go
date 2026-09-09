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

	"github.com/DiegohNY/costlane/internal/pricing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stdout, err)
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

// containsRate looks for the figure in several of the shapes a page might
// print it in. This is a smoke test, not a parser: its job is to notice that
// a number has vanished, and to say so without claiming more than it knows.
func containsRate(body, rate string) bool {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(rate, "0"), ".")
	for _, form := range []string{rate, trimmed, "$" + rate, "$" + trimmed} {
		if strings.Contains(body, form) {
			return true
		}
	}
	return false
}

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
