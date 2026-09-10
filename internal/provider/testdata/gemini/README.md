# Captured Gemini streaming responses

Raw bytes from `POST /v1beta/models/{model}:streamGenerateContent?alt=sse`,
recorded on 2026-09-10 against the live API. Each `.sse` file opens with SSE
comment lines (`:` prefixed, ignored by any compliant parser) giving the
capture date, the endpoint, the model and the exact request body.

These are the specification the Gemini stream translator is written against,
and the shape the fake provider must reproduce. **The fake conforms to these
captures, never the other way round.** A fixture is only ever replaced by
another capture.

No credential appears in any file: the API key travelled in an
`x-goog-api-key` header, which is not recorded, and the URL carries no key.
Checked with a literal search for the key used, and for `AIza`, `AQ.Ab`,
`authorization` and `?key=`.

## What the captures settled

**The documentation and the API disagree, and the API wins.** The
`migrate-to-interactions` page shows `streamGenerateContent` emitting
`content.start` / `content.delta` / `content.stop` events, and the newer
`/v1beta/interactions` endpoint uses `interaction.*` / `step.*` with usage
under `total_input_tokens` / `total_output_tokens` / `total_thought_tokens`.
Neither is what the endpoint actually returned. Every capture below is a
stream of `data:` frames carrying whole `GenerateContentResponse` objects —
the same shape as the non-streaming reply.

**Usage is cumulative and present on every chunk**, not only the last. The
field names are the non-streaming ones: `promptTokenCount`,
`candidatesTokenCount`, `thoughtsTokenCount`, `totalTokenCount`,
`promptTokensDetails`, `serviceTier`. A translator should take the last
chunk's figures rather than summing.

**`thoughtsTokenCount` stays constant while `candidatesTokenCount` grows** —
thinking finishes before output begins — and output must be counted as
`candidatesTokenCount + thoughtsTokenCount`, exactly as
`pricing.NormaliseGoogle` already does for the non-streaming path.

**Tool calls arrive whole, in one chunk.** `functionCall` carries a complete
`args` object and an `id`; there is no partial-JSON accumulation of the kind
Anthropic requires.

**There is no `[DONE]` sentinel.** The stream simply ends.

Chunks also carry `modelVersion`, `responseId`, and a `thoughtSignature` on
some parts.

## The fixtures

| File | Model | Chunks | Ends with | Notes |
|------|-------|--------|-----------|-------|
| `short-no-thinking.sse` | gemini-3.8-flash | 2 | `STOP` | `thinkingBudget: 0`; no `thoughtsTokenCount` in usage at all |
| `with-thinking.sse` | gemini-3.8-flash | 7 | `STOP` | 264 thinking tokens against 136 visible; the case that decides the usage field names |
| `tool-call.sse` | gemini-3.8-flash | 2 | `STOP` | one `functionCall`, complete in the first chunk |
| `long-output.sse` | gemini-3.8-flash | 80 | `MAX_TOKENS` | 2000-token ceiling reached; shows both a long stream and the truncation reason |

## What is not captured, and why

**A dedicated `maxOutputTokens: 5` capture.** The free tier allows 20
requests per day per model (`GenerateRequestsPerDayPerProjectPerModel-FreeTier`,
`quotaValue: 20`) and the budget ran out. The behaviour it was meant to show —
`finishReason: MAX_TOKENS` in a stream — is covered by `long-output.sse`,
which hit its own ceiling. A separate capture would show the same reason on a
two-chunk stream.

**Anything on `gemini-3.1-pro-preview`.** The free tier does not serve it:
the quota response reports `limit: 0` for both
`GenerateRequestsPerDayPerProjectPerModel-FreeTier` and
`GenerateContentInputTokensPerModelPerDay-FreeTier` on `gemini-3.1-pro`. This
is a refusal, not an exhaustion — no amount of waiting helps.

**Cached content.** `cachedContentTokenCount` does not appear in any capture,
because none of these requests used a cache. The translator handles the field
because the non-streaming path does; that branch is exercised by the fake
provider, not by a capture.

**An error mid-stream.** Not provoked. The fake provider covers it.

Anyone extending these should add captures rather than hand-written fixtures,
and record the gaps here the same way.
