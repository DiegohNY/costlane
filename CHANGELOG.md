# Changelog

Written by hand. A list generated from commit subjects tells you what was
touched; this tells you what changed for you.

## v0.1.0 — unreleased

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

Vertex AI and Bedrock are not supported. Google does not stream. Translation
covers chat completions and tool calls, not embeddings, images, audio, or the
Assistants and Responses APIs. There is one master credential and no user
accounts, no rate limiting, and no per-user attribution. Budgets are monthly
on UTC. No caching, no failover, no frontend.

### Verification

Live-provider verification — that a cancelled stream stops the meter, and that
the token fields read are the ones the providers send — is recorded in
[docs/provider-verification.md](docs/provider-verification.md).

### Container image

```
ghcr.io/diegohny/costlane:v0.1.0
```

linux/amd64 and linux/arm64. The version is stamped at link time and reported
by `/healthz`.
