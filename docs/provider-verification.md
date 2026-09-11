# Provider verification

**Status at v0.2.0: all three checks run against Google. OpenAI and Anthropic
remain unverified for want of a credential.**

| Check | OpenAI | Anthropic | Google |
|---|---|---|---|
| (a) non-streaming counts | not verified | not verified | **verified** |
| (b) streaming counts | not verified | not verified | **verified** |
| (c) cancellation stops billing | **not verified** | **not verified** | **indicative, not conclusive** |

Read that table before reading anything else in this repository about
cancellation.

**Token counting is verified against the live Gemini API, streaming and not.**
Both agree to the token, and Cloud Monitoring independently confirms the pair.

**Cancellation is measured but not settled.** Google recorded **zero output
tokens** for a request cancelled after one chunk, ten hours after the fact.
That is consistent with generation stopping at the disconnect. It is not proof,
because the same reading would appear if the metric were only written when a
request completes, and this project is on a free tier where nothing is charged
and so no bill exists to compare against. The reasoning is set out under check
(c) below, with every query.

**OpenAI and Anthropic are not measured at all.** No funded credential. The
cancel default rests on documented behaviour for them, and an operator who
needs certainty over cost should set `disconnect_policy: drain` on the keys
routed there.

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
go test ./internal/providerverify/ -v -count=1 -timeout 20m
```

Override the model per provider with `COSTLANE_VERIFY_OPENAI_MODEL`,
`COSTLANE_VERIFY_ANTHROPIC_MODEL`, `COSTLANE_VERIFY_GOOGLE_MODEL`. Run one
check at a time with `COSTLANE_VERIFY_ONLY=a`, `b` or `c`, which matters on a
free tier that allows twenty requests per day per model.

**`-count=1` is not optional.** Go caches a passing test result, so a rerun
with identical inputs replays the previous output — timestamps, request ids
and all — without touching the provider. A retry loop will then report
success in a second and hand you the UTC window of an *older* run, which is
the one thing a dashboard lookup cannot survive. This was not hypothetical:
the first successful cancellation run below was reported by a retry an hour
after it actually happened.

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

The size of that request was meant to be the whole design of the check. A
dashboard reports a day in aggregate, so a cancelled request that was only ever
going to produce thirty tokens vanishes into everything else billed that day.
Asking for four thousand and taking one was supposed to make the two outcomes
unmistakable: about one token billed means generation stopped, about four
thousand means it did not.

**That premise does not survive a thinking model, and the first live run showed
why.** The first chunk arrives only once the model has finished thinking — 1
minute 35 seconds in — so 2566 of the 4000 tokens had already been produced
before a cancel was possible at all. The contrast is not 1 against 4000 but
2566 against 4000.

The check compensates rather than pretending otherwise: it records the
provider's own usage figures from the last chunk read before the cancel, so the
comparison is against a known quantity instead of an expectation. Removing the
problem at the source means being able to ask Gemini not to think, which
costlane currently cannot —
[#24](https://github.com/DiegohNY/costlane/issues/24).

For each provider the test prints, in one block: the UTC start timestamp, when
the first chunk arrived and how long it took, the UTC cancellation timestamp,
how long the upstream took to stop, the provider request id, the chunks read,
the `max_tokens` allowed, and the usage already counted at the moment of the
cancel.

Half of (c) cannot be automated. What was billed is read afterwards — from a
provider dashboard, or, as below, from Cloud Monitoring, which for Google turns
out to expose the token counts programmatically.

## Results

Run on: 2026-09-11 (UTC). Gateway version: v0.2.0.
Project: `gen-lang-client-0945570694`, free tier, model `gemini-3.8-flash`.

Row (a) was taken on 2026-09-10 and re-run on 2026-09-11; the figures below
are the later run.

### (a) Non-streaming counts

| Provider | Model | Our count (in/out) | Provider reported (in/out) | Match | Provider request id |
|----------|-------|--------------------|----------------------------|-------|---------------------|
| OpenAI | — | — | — | **not verified** | no funded credential at v0.1.0 |
| Anthropic | — | — | — | **not verified** | no funded credential at v0.1.0 |
| Google | gemini-3.8-flash | 9 / 173 | 9 / 173 | **exact** | none returned (see notes) |

The Gemini payload behind that row, as the provider sent it, at
2026-09-11T09:47:55Z:

```json
{"promptTokenCount":9,"candidatesTokenCount":1,"totalTokenCount":182,
 "promptTokensDetails":[{"modality":"TEXT","tokenCount":9}],
 "thoughtsTokenCount":172,"serviceTier":"standard"}
```

costlane counted `input=9, cached_read=0, output=173, reasoning=172`. Output is
173 because Gemini bills thinking tokens as output and reports them apart from
it: 1 visible token plus 172 of thinking. **A meter that read
`candidatesTokenCount` alone would have billed this request at under one
percent of what it cost.** That is the exact class of mistake this check exists
to catch, and it is the reason the comparison is made against the provider's
raw payload rather than against anything costlane produced.

### (b) Streaming counts

| Provider | Model | Our count (in/out) | Reported (in/out) | Match | Provider request id |
|----------|-------|--------------------|-------------------|-------|---------------------|
| OpenAI | — | — | — | **not verified** | no funded credential at v0.1.0 |
| Anthropic | — | — | — | **not verified** | no funded credential at v0.1.0 |
| Google | gemini-3.8-flash | 12 / 329 | 12 / 329 | **exact** | none returned |

Taken at 2026-09-11T09:48:32Z. 50 visible output tokens and 279 thought ones,
folded into 329 by the same rule as row (a) — and by the same code, since the
streaming translator delegates its arithmetic to `pricing.NormaliseGoogle`
rather than keeping a second copy of it.

**Cloud Monitoring confirms both rows from a third source.** Google's own
`generate_content_usage_output_token_count` for that minute reads exactly
**502** for `thinking_enabled=true`, which is 173 + 329. Neither figure came
from Google's metric and the metric was not consulted until afterwards, so the
agreement is three independent readings of the same two requests: our meter,
the provider's per-response payload, and the provider's own telemetry.

### (c) Billing stops on cancellation

| Provider | Started (UTC) | Cancelled (UTC) | Stopped within | Chunks read | Output tokens Google recorded | Verdict |
|----------|---------------|-----------------|----------------|-------------|-------------------------------|---------|
| OpenAI | — | — | — | — | — | **not verified** |
| Anthropic | — | — | — | — | — | **not verified** |
| Google | 2026-09-11T10:02:00Z | 2026-09-11T10:03:35Z | <1 ms | 1 | **0** | **indicative, not conclusive** |

What the run itself recorded, from the last chunk read before the cancel:

```json
{"promptTokenCount":78,"candidatesTokenCount":10,"totalTokenCount":2644,
 "promptTokensDetails":[{"modality":"TEXT","tokenCount":78}],
 "thoughtsTokenCount":2556,"serviceTier":"standard"}
```

The first chunk took **1 minute 35 seconds** to arrive, because the model
thought before emitting anything. By the time a cancel was possible at all,
**2566 output tokens of a 4000 ceiling had already been produced** — 64% of the
budget the request was allowed. The check was designed around a contrast
between roughly one token and roughly four thousand; on a thinking model that
contrast does not exist, and the honest band narrowed to 2566 against 4000.
Closing that properly needs
[#24](https://github.com/DiegohNY/costlane/issues/24), which would let a
request ask Gemini not to think.

#### What Google's own telemetry says

Read from Cloud Monitoring at 2026-09-11T20:20Z — ten hours after the run, so
ingestion lag is not a factor.

**Output tokens for the whole day, `gemini-3.8-flash`:**

| Window (UTC) | thinking | Output tokens |
|---|---|---|
| 09:46:56 → 09:47:56 | false | 4 |
| 09:47:50 → 09:48:50 | true | 502 |

There is **no point at all** covering 10:02. The cancelled request contributed
nothing to this metric.

**But the same request is present in the input-token meter:**

| Window (UTC) | Input tokens |
|---|---|
| 10:01:55 → 10:02:55 | 67 |

And in the request counter:

| Window (UTC) | Requests |
|---|---|
| 10:02:03 → 10:03:03 | 1 |

So Google saw the request, counted it, and metered its input — and recorded
zero output tokens for it.

#### Why this is indicative and not a verdict

Two readings fit the same numbers, and nothing here separates them:

1. **Generation stopped at the disconnect.** Nothing further was produced and
   nothing was charged. The cancel default is correct for Gemini.
2. **Output usage is only written when a request completes.** The 2566 tokens
   we watched the model produce were generated and simply never recorded,
   because the request ended without completing. The metric would read zero
   either way.

The second reading is not far-fetched: the six requests that failed with 503
earlier the same morning also show input tokens and a request count with no
output tokens, which is what "recorded at admission, not at completion" would
look like.

Settling it needs a **bill**, and a free-tier project produces none. On a
project with billing enabled,
`generate_content_usage_output_token_count` is the usage meter an invoice
follows, and a zero there would mean not charged. That is the experiment that
would close this, and it costs whatever the tokens cost.

**Recorded verdict: indicative.** The evidence is consistent with the cancel
default being correct for Gemini and contains nothing against it, but it is
not the measurement this document set out to take, and calling it one would be
the kind of rounding this file exists to refuse.

#### Queries, so this is reproducible

```bash
# Which project, and is the metric there at all
gcloud projects list
gcloud services enable monitoring.googleapis.com --project PROJECT_ID
TOKEN=$(gcloud auth print-access-token)

# The metric descriptors this service publishes
curl -sG "https://monitoring.googleapis.com/v3/projects/PROJECT_ID/metricDescriptors"   -H "Authorization: Bearer $TOKEN"   --data-urlencode 'filter=metric.type = starts_with("generativelanguage.googleapis.com")'

# Output tokens, by model, with thinking split out
curl -sG "https://monitoring.googleapis.com/v3/projects/PROJECT_ID/timeSeries"   -H "Authorization: Bearer $TOKEN"   --data-urlencode 'filter=metric.type = "generativelanguage.googleapis.com/generate_content_usage_output_token_count"'   --data-urlencode 'interval.startTime=2026-09-11T00:00:00Z'   --data-urlencode 'interval.endTime=2026-09-11T20:20:00Z'   --data-urlencode 'view=FULL'

# Input tokens and request count, to tell "not produced" from "not recorded"
#   .../quota/generate_content_free_tier_input_token_count/usage
#   .../quota/generate_content_free_tier_requests/usage
```

The output metric carries labels `model`, `output_modality` and
`thinking_enabled`, which is what makes the thinking half separable at all.

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

**A 503 still spends a request against the daily quota.** Six attempts that
failed with 503 between 09:51 and 09:57 each show up in
`quota/generate_content_free_tier_requests/usage` and each consumed 67 input
tokens. On a twenty-request allowance that is nearly a third of a day spent on
failures, which is the reason the retry loop has a cap and stops rather than
draining it.

**Go caches a passing test, and a cached result carries stale timestamps.**
The first successful cancellation run was reported an hour after it happened,
by a retry that never touched the network. The UTC window in that output was
the older run's, and a lookup against the wrong window is worse than no lookup.
Hence `-count=1` in the documented command.

## If a check fails

A failure of (a) or (b) is a bug in an adapter and blocks the release.

A failure of (c) is worse: it means the cancel default is wrong for that
provider, and the honest response is to document it in the README's known
limitations and to recommend `disconnect_policy: drain` for keys routed there.
Silently keeping a default that overspends would be the one failure this
product cannot excuse.
