# 0001 — Cancel the upstream call when a client disconnects

Status: accepted. Supersedes the original "always drain" position.

## Context

A streamed completion can outlive the client that asked for it: a browser tab
closes, a mobile connection drops, a request is cancelled by a timeout in a
caller's own SDK. The gateway is then holding an upstream stream that nobody
is reading.

Two things can be done with it. Keep reading until the provider sends its
final usage event, which yields an exact token count. Or close the upstream
connection immediately, which means the last count the gateway has is the one
it accumulated from the chunks that arrived.

The original design chose to drain, on the grounds that a cost-control product
must never guess at a figure.

## Decision

Cancel by default. Draining stays available per key, as
`disconnect_policy: drain`.

The reversal came from noticing what draining actually buys. Providers stop
generating when the connection closes, and bill for what they produced. A
drain therefore does not observe a cost that already exists — it *creates*
one, by asking the provider to keep generating tokens that no one will ever
read, purely so the gateway can write down an exact number for them.

For a product whose purpose is to keep spending down, paying money to improve
the precision of a spending report is the wrong trade. The exact figure is
available to anyone who wants it, at its true price, by setting the policy on
that key.

## Alternatives rejected

**Always drain.** Exact accounting, paid for in tokens nobody reads. Rejected
because the cost is unbounded: an abandoned request with a large `max_tokens`
can run to completion at full price.

**Drain with a short timeout.** Cheaper, and still wrong in the same
direction, with the added defect that the figure is exact only sometimes and
nothing in the record says which times.

**Cancel with no option to drain.** Simpler, but there are legitimate reasons
to want exactness — chargeback to a customer, a disputed invoice — and the
per-key setting costs one column.

## Consequences

- The usage record for a cancelled stream carries the in-flight count, and is
  flagged as such: the source is `estimate`, never `provider`.
- Cancellation is bounded by the same reservation that every request takes, so
  a stream that disappears cannot leave budget held.
- The behaviour rests on providers stopping generation when a connection
  closes. That is documented behaviour, and it is verified against live APIs
  in [provider verification](../provider-verification.md); it is the single
  assumption in the design that a fake provider cannot settle.
