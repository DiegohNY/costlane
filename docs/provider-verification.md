# Provider verification

**Status: not yet run.** Awaiting funded credentials for OpenAI, Anthropic and
Google. No release should be tagged before the table below is filled in.

Everything else in this repository is tested against a fake provider. That
proves the gateway does what it intends; it cannot prove that what it intends
matches what the real APIs do. Two assumptions can only be settled by spending
real money:

1. **A cancelled stream stops the meter.** The default disconnect policy closes
   the upstream connection when a client goes away, on the belief that the
   provider stops generating and stops charging
   ([ADR 0001](adr/0001-cancel-not-drain.md)). If that belief is wrong, the
   gateway under-reports every abandoned stream.
2. **The token fields we read are the ones they send.** Our cost is computed
   from the provider's own usage figures. If a field is renamed, nested
   differently, or split into a new billable class, the meter silently reads
   the wrong number.

## Method

The harness lives in [`internal/providerverify`](../internal/providerverify).
It talks to the provider adapters directly, so it needs credentials and a
network but no database.

```bash
COSTLANE_VERIFY_PROVIDERS=1 \
COSTLANE_OPENAI_API_KEY=... \
COSTLANE_ANTHROPIC_API_KEY=... \
COSTLANE_GOOGLE_API_KEY=... \
go test ./internal/providerverify/ -v -timeout 20m
```

Override the model per provider with `COSTLANE_VERIFY_OPENAI_MODEL`,
`COSTLANE_VERIFY_ANTHROPIC_MODEL`, `COSTLANE_VERIFY_GOOGLE_MODEL`.

Three checks run against each configured provider:

**(a) Counting, non-streaming.** One small completion. The adapter's parsed
counts are compared against a second, independent parse of the raw response
body by the field names each dialect documents. They must agree to the token.

**(b) Counting, streaming.** One streamed completion, relayed through the real
pump. The counts accumulated while the stream ran are compared against the
provider's own final usage event, read independently from a tee of the raw
upstream bytes. They must agree to the token.

**(c) Cancellation.** A completion is requested with `max_tokens: 4000` and a
prompt written to reach it — three thousand words on the history of the metric
system. **One chunk** is read and the context is cancelled, which is exactly
what a client closing its connection does, as early as it can be done. The test
asserts the upstream body stops delivering promptly.

The size of that request is the whole design of the check. A dashboard reports
a day in aggregate, so a cancelled request that was only ever going to produce
thirty tokens vanishes into everything else billed that day and proves nothing.
Asking for four thousand and taking one makes the two outcomes unmistakable:

- **roughly one token of output billed** → generation stopped at the
  disconnect, and the cancel default is correct.
- **roughly four thousand** → the provider kept generating after the connection
  closed, and the default is wrong for it.

For each provider the test prints, in one block: the UTC start timestamp, the
UTC cancellation timestamp, how long the upstream took to stop, the provider
request id, the chunks read before the cancel, and the `max_tokens` the request
allowed. Those are the six things needed to find the request on a dashboard and
say what it should have cost.

Half of (c) cannot be automated. Billing is confirmed by a human opening the
provider's usage dashboard, filtering to the UTC window the test printed,
finding the request by id where the provider exposes one, and reading the
output tokens charged. That confirmation is recorded by hand below.

## Results

Run on: _pending_
Gateway version: _pending_

### (a) Non-streaming counts

| Provider | Model | Our count (in/out) | Reported (in/out) | Match | Provider request id |
|----------|-------|--------------------|-------------------|-------|---------------------|
| OpenAI | | | | | |
| Anthropic | | | | | |
| Google | | | | | |

### (b) Streaming counts

| Provider | Model | Our count (in/out) | Reported (in/out) | Match | Provider request id |
|----------|-------|--------------------|-------------------|-------|---------------------|
| OpenAI | | | | | |
| Anthropic | | | | | |
| Google | | | | | |

Google has no streaming adapter today; that row is expected to read
"not applicable" rather than to pass.

### (c) Billing stops on cancellation

Ceiling asked for in every row: `max_tokens: 4000`. Cancelled after one chunk.

| Provider | Started (UTC) | Cancelled (UTC) | Stopped within | Provider request id | Chunks read | Dashboard: output tokens billed | Verdict |
|----------|---------------|-----------------|----------------|---------------------|-------------|---------------------------------|---------|
| OpenAI | | | | | | | |
| Anthropic | | | | | | | |
| Google | | | | | | | |

Verdict is **stopped** when the billed output is near the chunks read, and
**kept generating** when it is near 4000. Anything in between goes in the notes
with the figure, not rounded to whichever verdict is more convenient.

Dashboard checked by: _pending_ — on: _pending_

### Notes

_Anything surprising goes here: a dialect that differs from its documentation,
a dashboard that reports with a delay, a provider that keeps generating after
the connection closes._

## If a check fails

A failure of (a) or (b) is a bug in an adapter and blocks the release.

A failure of (c) is worse: it means the cancel default is wrong for that
provider, and the honest response is to document it in the README's known
limitations and to recommend `disconnect_policy: drain` for keys routed there.
Silently keeping a default that overspends would be the one failure this
product cannot excuse.
