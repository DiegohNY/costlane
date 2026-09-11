# Changelog

Written by hand. A list generated from commit subjects tells you what was
touched; this tells you what changed for you.

## v0.2.1 — 2026-09-11

### Fixed

- **Gemini models were not routable from the binary.** `buildRouter` in
  `cmd/costlane` registered OpenAI and Anthropic and never constructed a
  Google provider. Configuration read `COSTLANE_GOOGLE_API_KEY`, the redactor
  scrubbed it, the README documented it, and the price seed mapped every
  Gemini model to a provider named `google` that did not exist in the router —
  so every Gemini request answered 404, and a deployment with only a Google
  credential refused to start with a message that did not mention Google.

  **This was live in v0.1.0 and v0.2.0.** If you configured Gemini on either,
  it never worked. v0.2.0's headline feature, Gemini streaming, was reachable
  from the library and from the tests but not from the binary.

  Nothing caught it because nothing crossed that seam: the adapters, the
  streaming tests and the live provider verification all construct a provider
  directly, and the one end-to-end test that went through `buildRouter` asked
  for an OpenAI model. There are now three tests on `buildRouter` itself, and
  two checks that would have caught it at boot without calling any provider —
  `/v1/models` must list a model for every configured provider, in the release
  smoke test and again in the compose quickstart, which also streams a real
  Gemini request end to end.

- **The release smoke test never started the image.** It passed no provider
  credential, so the gateway refused to boot and the job failed for a reason
  unrelated to the image. It now supplies three fake keys, which are never
  used: listing models reads the price table and the router and talks to
  nobody.

  The gate itself behaved correctly throughout — `:latest` stayed on v0.1.0
  rather than moving to an image that had not passed its own boot check.

## v0.2.0 — 2026-09-11

> **Known issue, fixed in v0.2.1: Gemini models are not routable from the
> binary in this release.** The streaming adapter below is real and tested,
> but `cmd/costlane` never registered a Google provider, so every Gemini
> request answered 404. Use v0.2.1.

### Added

- **Gemini streaming.** `streamGenerateContent` is relayed chunk by chunk and
  metered exactly, so all three providers now stream. The translation is
  pinned by fixtures captured from the live API rather than written from the
  documentation, which describes two dialects that endpoint does not send.
  Tool calls, thinking tokens and the `MAX_TOKENS` truncation reason are all
  covered ([#12](https://github.com/DiegohNY/costlane/issues/12)).
- **A per-provider definition of a clean close.** OpenAI ends with `[DONE]`,
  Anthropic with `message_stop`, Gemini by closing the connection after a
  chunk carrying a `finishReason`. Each adapter answers for its own protocol
  ([ADR 0008](docs/adr/0008-per-provider-clean-close.md)).
- **`:latest` moves only after the image has booted.** A release now pushes
  the version tag, starts that exact image against a real Postgres, waits for
  `/readyz` and checks that `/healthz` reports the tag — and only then
  re-points `:latest`, by copying the manifest that was tested rather than by
  building a second time. A release that fails its own boot check leaves
  `:latest` where it was.
- **Exact accounting for a cancelled Gemini stream.** Gemini restates its
  running totals on every chunk, so the last one read before a disconnect is
  the provider's own figure. Those records say `usage_source = provider`
  rather than `tokenizer`. A property of that one protocol, and only about
  what we can write down — not about whether Google stops charging.

### Known limitations

Two of v0.1.0's limitations survive this release, and one does not.

- **Resolved: Gemini streams.** A streamed request to a Gemini model is
  relayed and metered like any other, as of this release.
- **Partly closed — the cancel default is now measured on Gemini.** A request
  cancelled after one chunk recorded zero output tokens in Google's own
  telemetry, which is consistent with generation stopping at the disconnect.
  It is recorded as indicative rather than conclusive, and the reasoning is in
  [provider verification](docs/provider-verification.md). OpenAI and Anthropic
  remain unmeasured for want of a credential; set `disconnect_policy: drain`
  on keys routed there if you need certainty —
  [#13](https://github.com/DiegohNY/costlane/issues/13).
- **Still open — costlane cannot ask Gemini to stop thinking.** No path from
  the OpenAI dialect to `thinkingConfig`, so a caller pays for a thinking
  budget they cannot set. Google bills thinking as output: a one-word answer
  measured 173 output tokens, 172 of them thought
  ([#24](https://github.com/DiegohNY/costlane/issues/24)).
- **Still open — Vertex AI is not seeded and not supported.** Google means the
  Gemini API with an API key. Vertex needs a different auth flow, a different
  URL shape, and rates that have historically diverged from the Gemini API's;
  an unverifiable rate does not enter the price table. Bedrock likewise.

Everything else in v0.1.0's list stands unchanged.

### Fixed

Four defects that were live in v0.1.0. **If you ran v0.1.0, the first two
affected you.**

- **Requests with thinking were flagged `partially_priced` although their cost
  was correct.** Reasoning tokens are a breakdown of output, not a charge of
  their own — every provider that reports them counts them inside its output
  total already — but they sat in the pricing loop with no rate behind them.
  **Impact: cosmetic but misleading.** Every Gemini request that thought, and
  every OpenAI reasoning request, carried a flag saying its figure might be
  incomplete when it was exact. No money was miscounted. If you filtered or
  alerted on `partially_priced`, those alerts were false.
- **A client asking for usage on an Anthropic stream never received the
  chunk.** `stream_options.include_usage` was read by the OpenAI adapter
  alone, and the proxy drops a stream's trailing chunks unless the client is
  known to have asked — so the usage chunk Anthropic's translator assembles
  was built and discarded on every request. **Impact: real.** Any client
  relying on the usage chunk of a streamed Anthropic response got nothing and
  had to guess. costlane's own accounting was unaffected: it reads those
  figures on a different path.
- **Negative token counts from a provider passed straight into the meter.**
  `Table.Cost` refuses a negative count with an error that callers log and
  swallow into a zero cost, so an absurd figure upstream would have become a
  free request. Now clamped to zero and marked degraded in all three
  normalisers and in the Anthropic stream translator. **Impact: none
  observed.** No provider is known to have sent one; this was found by
  fuzzing, not by an invoice.
- **The fake provider's Gemini streaming dialect matched neither the captures
  nor the documentation.** Nothing had ever exercised it, which is how it
  drifted. **Impact: tests only.** It never ran in production, but it means
  any confidence drawn from Gemini streaming tests before this release was
  worth less than it looked.

## v0.1.0 — 2026-09-10

The first release. costlane proxies chat completions to OpenAI, Anthropic and
Google, meters every request exactly, and refuses one that would take a
virtual key past its budget.

### What it does

- **OpenAI-compatible proxy.** Point an existing SDK at it by changing
  `base_url`. Request bodies reach OpenAI-compatible providers byte for byte.
- **Hard spend limits, enforced in the request path.** A key over budget gets
  `402 Payment Required` before the provider is called. The limit holds under
  concurrency: the check is a predicate inside the statement that takes the
  reservation, so a thousand parallel requests on one key cannot overspend it.
- **Exact accounting, including streaming.** Cost comes from the provider's
  own usage figures, read as the stream goes past, never from a tokeniser
  guessing at them.
- **Virtual keys** with per-key budgets, model allowlists and disconnect
  policy, minted through an admin API.
- **A read API** for spend and usage, grouped by key, model, provider, day or
  hour, with amounts as decimal strings and windows cut on UTC boundaries.
- **Versioned prices**, one row per token kind and context tier, each carrying
  the URL it was read from and the date it was read. A daily job fails the
  build when a source page changes.
- **Prometheus metrics**, liveness and readiness probes, and a shutdown that
  reports unready before it stops accepting.

### Behaviour to know about before deploying

- A client that disconnects mid-stream **cancels** the upstream call by
  default; set a key's `disconnect_policy` to `drain` to pay for an exact
  figure instead.
- `max_tokens` is injected for Anthropic only, and reported in
  `X-Costlane-Injected-Max-Tokens`.
- An unpriced model is **refused** on a key that has a budget, and recorded
  with a NULL cost on one that does not.
- Prompt logging cannot be enabled without `COSTLANE_PROMPT_RETENTION`. The
  process refuses to start otherwise.
- A parameter with no equivalent in the destination provider is refused by
  name rather than silently dropped.

### Known limitations

Two of these are load-bearing enough to have issues of their own.

- **Gemini is non-streaming only.** There is no Gemini streaming adapter, so a
  streamed request to a Gemini model is refused by name rather than buffered.
  First item of v0.2 — [#12](https://github.com/DiegohNY/costlane/issues/12).
  *(Resolved in v0.2.0. Left standing here because this section records what
  v0.1.0 shipped, and editing it would make the entry lie about the release it
  describes.)*
- **The cancel default is not measured on any provider.** costlane closes the
  upstream connection when a client disconnects, on documented provider
  behaviour that has not been confirmed against a live API: OpenAI and
  Anthropic had no funded credential at release, and Google has no stream to
  cancel. If you need certainty over cost rather than a documented default, set
  `disconnect_policy: drain` on the keys concerned —
  [#13](https://github.com/DiegohNY/costlane/issues/13).

The rest, in one paragraph: Vertex AI and Bedrock are not supported.
Translation covers chat completions and tool calls, not embeddings, images,
audio, or the Assistants and Responses APIs. There is one master credential and
no user accounts, no rate limiting, and no per-user attribution. Budgets are
monthly on UTC. No caching, no failover, no frontend.

### Verification

Token counting was checked against a live Gemini API and agrees to the token,
including the thinking tokens Gemini bills as output and reports separately.
Everything else is tested against a fake provider that reproduces each dialect.
What was checked and what was not — with the raw provider payload behind the
one row that passed — is in
[docs/provider-verification.md](docs/provider-verification.md).

### Container image

```
ghcr.io/diegohny/costlane:v0.1.0
```

linux/amd64 and linux/arm64. The version is stamped at link time and reported
by `/healthz`.
