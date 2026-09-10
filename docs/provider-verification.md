# Provider verification

**Status at v0.2.0: one check verified on one provider; two more are queued
against a quota that resets on 2026-09-11 at 07:00 UTC.**

| Check | OpenAI | Anthropic | Google |
|---|---|---|---|
| (a) non-streaming counts | not verified | not verified | **verified** |
| (b) streaming counts | not verified | not verified | **pending** |
| (c) cancellation stops billing | **not verified** | **not verified** | **pending** |

Read that table before reading anything else in this repository about
cancellation. **No provider has been measured for check (c) yet.** OpenAI and
Anthropic have no funded credential. Google could not be run at all before
v0.2.0, for want of a streaming adapter; that adapter now exists, and the two
remaining checks are queued rather than blocked.

*Pending* means the harness is ready, the credential exists, and the run is
waiting on the free tier's daily allowance —
`GenerateRequestsPerDayPerProjectPerModel-FreeTier`, twenty requests per day
per model, spent on 2026-09-10 capturing the streaming fixtures. It resets at
midnight Pacific, which is **2026-09-11 07:00 UTC**.

Until those rows are filled in, the cancel default rests on documented
provider behaviour rather than on a measurement taken here.

An operator who needs certainty over cost rather than a documented default
should set `disconnect_policy: drain` on the keys concerned, and pay for an
exact figure.

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

Run on: 2026-09-10 (UTC)
Gateway version: v0.1.0

### (a) Non-streaming counts

| Provider | Model | Our count (in/out) | Provider reported (in/out) | Match | Provider request id |
|----------|-------|--------------------|----------------------------|-------|---------------------|
| OpenAI | — | — | — | **not verified** | no funded credential at v0.1.0 |
| Anthropic | — | — | — | **not verified** | no funded credential at v0.1.0 |
| Google | gemini-3.8-flash | 9 / 122 | 9 / 122 | **exact** | none returned (see notes) |

The Gemini payload behind that row, as the provider sent it:

```json
{"promptTokenCount":9,"candidatesTokenCount":1,"totalTokenCount":131,
 "promptTokensDetails":[{"modality":"TEXT","tokenCount":9}],
 "thoughtsTokenCount":121,"serviceTier":"standard"}
```

costlane counted `input=9, cached_read=0, output=122, reasoning=121`. Output is
122 because Gemini bills thinking tokens as output and reports them apart from
it: 1 visible token plus 121 of thinking. A meter that read
`candidatesTokenCount` alone would have billed this request at under one
percent of what it cost. That is the exact class of mistake this check exists
to catch, and it is the reason the comparison is made against the provider's
raw payload rather than against anything costlane produced.

### (b) Streaming counts

| Provider | Model | Our count (in/out) | Reported (in/out) | Match | Provider request id |
|----------|-------|--------------------|-------------------|-------|---------------------|
| OpenAI | — | — | — | **not verified** | no funded credential at v0.1.0 |
| Anthropic | — | — | — | **not verified** | no funded credential at v0.1.0 |
| Google | gemini-3.8-flash | | | **pending** — quota reset 2026-09-11 07:00 UTC | |

### (c) Billing stops on cancellation

Ceiling asked for in every row: `max_tokens: 4000`. Cancelled after one chunk.

| Provider | Started (UTC) | Cancelled (UTC) | Stopped within | Provider request id | Chunks read | Dashboard: output tokens billed | Verdict |
|----------|---------------|-----------------|----------------|---------------------|-------------|---------------------------------|---------|
| OpenAI | — | — | — | — | — | — | **not verified** |
| Anthropic | — | — | — | — | — | — | **not verified** |
| Google | | | | | | | **pending** — quota reset 2026-09-11 07:00 UTC |

Verdict would be **stopped** when the billed output is near the chunks read,
and **kept generating** when it is near 4000. Anything in between goes in the
notes with the figure, not rounded to whichever verdict is more convenient.

No verdict has been reached for any provider. For OpenAI and Anthropic the
reason is a missing credential; for Google it is a daily quota already spent,
and one day of waiting. Until v0.2.0 there was a third reason on the Google
row — check (c) needs a stream to cancel and the Gemini adapter only completed
— and that one is now gone.

**How the Google run will go, so that a rushed one is recognisable as such.**
Three requests, one per check. Retries on the 503s that model returns, spaced
thirty seconds apart, twelve attempts in total across the whole run. If twelve
are not enough the run stops and says so, rather than draining the day's
allowance to zero a second time — an empty quota costs a day, and there is no
figure worth that.

Gemini has a property worth noting before the run happens: it restates its
running totals on every chunk, so the last chunk read before a disconnect is
already the provider's exact count. Our side of the check will therefore be a
measurement rather than an estimate. It says nothing about whether Google stops
charging, which is the half only a dashboard can answer.

### What this means for the release

v0.1.0 ships with the cancel default **unmeasured**. That is a smaller claim
than the one the design makes, and the README's known limitations say so, along
with the remedy: `disconnect_policy: drain` on any key where an exact figure
matters more than the tokens it costs to obtain.

Closing this gap needs funded credentials on OpenAI and Anthropic
([#13](https://github.com/DiegohNY/costlane/issues/13)). The streaming adapter
for Gemini ([#12](https://github.com/DiegohNY/costlane/issues/12)) landed in
v0.2, so a Google credential is now enough to run every check against it:

```bash
COSTLANE_VERIFY_PROVIDERS=1 COSTLANE_GOOGLE_API_KEY=...   go test ./internal/providerverify/ -v -timeout 20m
```

One caveat for whoever runs it. The free tier allows twenty requests per day
per model, and each of the three checks spends one; the cancellation check asks
for four thousand output tokens, which is well inside the free input allowance
but will show up on the dashboard as the largest of the three. A day's budget
is enough for the run and for two retries, which the 503s that model returns
make likely.

What v0.1.0 does establish is narrow and real: for Gemini, costlane reads the
provider's own token figures correctly, including the thinking tokens that are
billed as output and reported separately.

Dashboard checked by: not applicable — no cancellation run produced a request
to look up.

### Notes

**The first version of this check verified nothing, and passed.** It compared
costlane's counts against the body `Complete` returns — which for Anthropic and
Google is already *translated into the OpenAI dialect by costlane*. Two halves
of the same code agreeing with each other is not a second opinion. Only OpenAI,
whose dialect passes through untouched, was ever genuinely compared. The check
now routes the adapter through a recording proxy (`upstreamRecorder`) that
keeps the bytes the provider actually sent, and compares against those. The
figures in the table above come from that version.

**A recording proxy must not forward `Accept-Encoding`.** Go's transport adds
the header itself and transparently decompresses the reply — but only when it
was the one to add it. Copying the client's header forward makes compression
the caller's business, and the recorder ends up holding gzip bytes instead of
the JSON it exists to read. The first run after the fix was still a failure,
with a body of binary noise, which is what pointed at it.

**Gemini returns no request id.** There is no `X-Request-Id` header on a
`generateContent` response, so `provider_request_id` is empty for that row and
a dashboard lookup has to go by timestamp. Nothing is wrong; it is simply not
offered.

**`gemini-3.8-flash` returns 503 UNAVAILABLE often.** "This model is currently
experiencing high demand." Ten consecutive attempts failed at one point and the
next succeeded. It is transient capacity, not authentication and not a wrong
model name — those return 401 and 404. Worth knowing before reading a failed
verification run as a defect.

## If a check fails

A failure of (a) or (b) is a bug in an adapter and blocks the release.

A failure of (c) is worse: it means the cancel default is wrong for that
provider, and the honest response is to document it in the README's known
limitations and to recommend `disconnect_policy: drain` for keys routed there.
Silently keeping a default that overspends would be the one failure this
product cannot excuse.
