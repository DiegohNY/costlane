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

**(c) Cancellation.** A long completion is requested, five chunks are read, and
the context is cancelled — exactly what a client closing its connection does.
The test asserts the upstream body stops delivering promptly, and logs the
provider request id, the start time, the cancellation time and the number of
chunks read.

Half of (c) cannot be automated. Billing is confirmed by opening the
provider's usage dashboard, finding the request by id or by timestamp, and
checking that the output tokens charged correspond to the handful of chunks
that were read rather than to the full completion that was asked for. That
confirmation is recorded by hand below.

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

| Provider | Chunks read | Upstream stopped within | Dashboard: output tokens billed | Consistent with a cancelled stream | Checked by / when |
|----------|-------------|-------------------------|---------------------------------|------------------------------------|-------------------|
| OpenAI | | | | | |
| Anthropic | | | | | |
| Google | | | | | |

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
