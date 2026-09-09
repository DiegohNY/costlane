# 0006 — Record "partially priced" separately from "unpriced"

Status: accepted. Supersedes a single `unpriced` boolean.

## Context

The price table can fail to price a request in two different ways, and the
first implementation had one boolean for both.

A model can be absent entirely: a new release the seed has not caught up with.
Nothing about that request can be priced, so the cost is *unknown* — recorded
as NULL, never as zero, because zero reads as free.

Or a model can be present but missing a rate for one of the kinds the response
actually used. A provider adds a new billable class — a cache write tier, a
reasoning-token rate — and a request that uses it has part of its cost
computable and part not.

With one boolean, that second case had to be forced into one of two answers.
Flagging it unpriced discards a cost that is real and known. Not flagging it
charges the known part and says nothing, which understates the spend silently
and lets a budget be exceeded by whatever the missing rate was worth.

## Decision

Two flags, on the pricing result and on the usage record.

- `unpriced`: nothing could be priced. Cost is NULL.
- `partially_priced`: some kinds were priced and others were not. The cost
  recorded is the sum of the known part, and the record names the kinds that
  were missing.

Charging the known part and saying so is the only fail-closed option: it never
overstates, it never silently understates, and the record carries enough to
reprice the request once the missing rate is added.

## Alternatives rejected

**One boolean, partial counts as unpriced.** Throws away money that was
genuinely spent and is genuinely known.

**One boolean, partial counts as priced.** Understates spend with nothing in
the record to reveal it. This is precisely the failure a budget product exists
to prevent.

**Refuse the request when any kind is unpriced.** Impossible: the missing kind
is discovered from the *response*, after the tokens have been bought.

## Consequences

- `usage_records` carries both flags, and a metric counts each, so an unpriced
  kind appearing in production is visible before it shows up in a
  reconciliation.
- A budget can be exceeded by the value of an unpriced kind. The exposure is
  bounded by how long the price table stays stale, which the daily pricing job
  bounds in turn.
- The reservation path is stricter than the settle path: an unpriced *model*
  on a budgeted key is refused outright, because a zero estimate would pass
  any limit.
