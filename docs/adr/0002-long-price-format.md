# 0002 — One row per rate, with its source and its date

Status: accepted. Supersedes the compact price map.

## Context

Prices have to live somewhere. The obvious shape is the one every pricing page
suggests: a map from model to a couple of numbers.

```yaml
gpt-6-astra: {input: 10, output: 50}
```

It is short, it is readable, and it was the first thing written.

## Decision

Use the long form: one row per (kind, tier, validity window), each carrying
the URL it was read from and the date it was read.

```yaml
- {kind: input, tier_to: 272000, usd_per_mtok: "10", source_url: ..., fetched_at: ...}
```

The compact form collapsed under the first real pricing page. Providers price
cached reads differently from fresh input, price cache writes by duration,
price above a context threshold at a different rate, and change all of it
without warning. A map of two numbers per model cannot express any of that,
and the workarounds — a nested map here, a special case there — reinvent the
long form one field at a time, without its discipline.

The per-row `source_url` and `fetched_at` are the part that matters most. A
rate with no provenance cannot be audited, and pricing is the one part of this
system where being wrong is indistinguishable from lying.

## Alternatives rejected

**A compact map with special cases.** What the long form replaced. Rejected
because each new pricing behaviour needed a new escape hatch, and none of them
could be checked against anything.

**Prices in the database.** A second source of truth that can disagree with
the one actually in use. Left out until something needs it; the YAML is
embedded in the binary and loaded into an in-memory snapshot.

**Fetching prices from provider APIs.** No provider publishes a machine
readable price list that covers cache tiers and context thresholds. A scraper
would be a source of silent, confident errors.

## Consequences

- A rate that is missing is *unpriced*, never zero. See [ADR 0006](0006-partially-priced.md).
- A daily CI job re-fetches every `source_url` and fails when a page changes,
  so a price change is noticed by a red build rather than by an invoice.
- The seed file is long, and reads like an audit trail. That is the intent.
