# Changelog

Written by hand. A list generated from commit subjects tells you what was
touched; this tells you what changed for you.

## v0.2.0 — unreleased

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
- **Exact accounting for a cancelled Gemini stream.** Gemini restates its
  running totals on every chunk, so the last one read before a disconnect is
  the provider's own figure. Those records say `usage_source = provider`
  rather than `tokenizer`. A property of that one protocol, and only about
  what we can write down — not about whether Google stops charging.

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
