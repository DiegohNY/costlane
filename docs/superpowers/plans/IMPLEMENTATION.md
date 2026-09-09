# costlane — Implementation Plan

**Spec:** [2026-09-09-costlane-design.md](../specs/2026-09-09-costlane-design.md)
**Rule:** TDD throughout — failing test first, implementation second.
**Rule:** nothing is "done" until every box in the phase is ticked.
**Rule:** each phase ends with a review checkpoint and waits for approval.

Legend: `[ ]` todo · `[x]` done · `[~]` in progress · `[!]` blocked

---

## F0 — Scaffolding and CI

Goal: an empty but fully wired repository where a red test blocks a merge.

- [x] `git init`, `go.mod` (module `github.com/DiegohNY/costlane`, go 1.27)
- [x] Apache-2.0 `LICENSE` (no NOTICE: no third-party attributions yet)
- [ ] Package skeleton per spec §3, each with a doc.go stating its purpose
- [ ] `internal/config`: env to struct, no defaults hidden in code
- [ ] Boot validation: `drain_timeout <= provider_timeout`; `TTL >= provider_timeout + drain_timeout + margin`
- [ ] TEST: config validation rejects each invalid combination
- [ ] `.golangci.yml` with errcheck, bodyclose, gosec, staticcheck
- [ ] CI: lint job
- [ ] CI: `go test ./... -race`
- [ ] CI: build + docker build
- [ ] `Dockerfile`, distroless base, non-root user
- [x] README skeleton with the agreed tagline
- [x] `.gitignore` (Go, .env*, *.pem, .DS_Store) + manual secret scan of history
- [x] Public repo `DiegohNY/costlane` created and pushed
- [x] Branch protection on main (PR required, no force push, no deletion)
- [ ] Attach CI jobs as required status checks (once they have run once)
- [ ] VERIFY: a deliberately failing test blocks the merge
- [ ] REVIEW CHECKPOINT — wait for approval

## F1 — Storage, migrations, schema

Goal: the schema exists and its invariants are enforced by Postgres.

- [ ] goose as a library, `.sql` files via `embed.FS`
- [ ] Advisory lock around migration application
- [ ] `COSTLANE_MIGRATE_ON_BOOT` flag (default true)
- [ ] Migration: `CREATE EXTENSION btree_gist` (annotated; may need privileges)
- [ ] Migration: `virtual_keys` per spec §7.3
- [ ] Migration: `key_budgets` per spec §5.2
- [ ] Migration: `budget_reservations` + partial index on pending
- [ ] Migration: `model_prices` + EXCLUDE constraint per spec §6.1
- [ ] Migration: `model_aliases` + EXCLUDE constraint per spec §6.2
- [ ] Migration: `usage_records`, BRIN on created_at, btree on (key_id, created_at)
- [ ] Migration: `request_payloads` (separate table, droppable)
- [ ] Separate read/write pools; `statement_timeout` on the read pool
- [ ] testcontainers harness with automatic skip when Docker is absent
- [ ] TEST: migrations apply cleanly from empty, and are idempotent
- [ ] TEST: overlapping price rows rejected by EXCLUDE
- [ ] TEST: overlapping alias rows rejected by EXCLUDE
- [ ] TEST: two concurrent migrators, advisory lock holds
- [ ] CI: integration job (testcontainers)
- [ ] REVIEW CHECKPOINT — wait for approval

## F2 — Pricing and aliases

Goal: given a model and a token count, the exact cost — or an explicit refusal.

- [ ] YAML schema for prices and aliases
- [ ] `pricing/*.yaml` seeded for OpenAI, Anthropic, Google, with source_url
- [ ] Boot loader YAML to Postgres, idempotent, fails on conflicting rewrite
- [ ] In-memory snapshot via atomic.Pointer, replaced wholesale
- [ ] `POST /admin/pricing/reload`
- [ ] Exact model matching — no startswith, no substring
- [ ] Alias resolution, historised, exact at each step
- [ ] Context-tier selection (input_tokens_from / _to)
- [ ] Four-way cost arithmetic (input, cached_read, cache_write, output)
- [ ] OpenAI caching normalisation: subtract cached_tokens from prompt_tokens
- [ ] Anthropic caching normalisation: separate cache_read / cache_creation
- [ ] Fail-closed: missing row or null rate means unknown, never free
- [ ] TEST: cost arithmetic across all four token classes
- [ ] TEST: `gpt-4` must not match `gpt-4o`
- [ ] TEST: alias resolving to different snapshots at different dates
- [ ] TEST: tier boundary, exactly at the edge
- [ ] TEST: caching normalisation reconciles with each provider's total
- [ ] TEST: unknown model yields unknown, never zero cost
- [ ] CI: weekly pricing source verification, opens an issue, never fails the build
- [ ] CI: unreachable page is reported as unverifiable, not as verified
- [ ] REVIEW CHECKPOINT — wait for approval

## F3 — Auth and virtual keys

Goal: a key authenticates in one round-trip and revocation takes effect at once.

- [ ] Key generation `cl_<32 bytes base62>`, crypto/rand
- [ ] SHA-256 hashing, unique index, `key_prefix` for display
- [ ] Master key from env, constant-time comparison
- [ ] `POST /admin/keys` — secret returned exactly once
- [ ] `GET /admin/keys` — never returns secrets
- [ ] `PATCH /admin/keys/{id}` — budget, allowed_models, disconnect_policy
- [ ] PATCH validates `drain_timeout <= provider_timeout` per key
- [ ] `DELETE /admin/keys/{id}` — soft revoke
- [ ] Scope type: repository forces `WHERE key_id` for a virtual key
- [ ] TEST: secret is not retrievable after creation
- [ ] TEST: revoked key rejected immediately, no cache window
- [ ] TEST: a virtual key cannot read another key's usage
- [ ] TEST: scope parameter is non-optional (compile-time)
- [ ] REVIEW CHECKPOINT — wait for approval

## F4 — Budget: reserve, settle, reaper

Goal: a thousand concurrent requests never breach one counter.

- [ ] Fused reserve statement: auth + allowed_models + rotation + reserve
- [ ] Lazy UTC month rotation inside the reserve
- [ ] `limit_usd IS NULL` means unlimited
- [ ] Reservation INSERT in the same transaction, window from RETURNING
- [ ] Diagnostic SELECT on zero rows: classify 401 / 402 / 403
- [ ] Cheap reserve estimate: chars/4 + max_tokens (or 4096 default)
- [ ] Settle: one transaction, state always terminal
- [ ] Settle reads old state via `settled_after_expiry` in SET, not RETURNING state
- [ ] Settle releases reserved conditionally (no double release after expiry)
- [ ] Settle adds to spent only when window matches
- [ ] Overshoot flag + metric; no `max_tokens` injection
- [ ] Reaper: single CTE, state change and release together
- [ ] Reaper uses FOR UPDATE SKIP LOCKED, safe for N replicas
- [ ] Three reconciliation queries + `GET /admin/reconcile`
- [ ] TEST: N concurrent reserves on one key; spent+reserved <= limit; exact success count
- [ ] TEST: double settle counts once
- [ ] TEST: settle after expiry — no double release, flag set
- [ ] TEST: retried settle after crash does not double-count
- [ ] TEST: reaper versus settle race, exactly one wins
- [ ] TEST: reservation spanning month boundary lands in its own window
- [ ] TEST: unlimited key (NULL limit) is never refused
- [ ] TEST: all three reconciliations return zero after concurrent load
- [ ] BENCH: `BenchmarkReserveContention`
- [ ] REVIEW CHECKPOINT — wait for approval

## F5 — Fake provider and proxy, non-streaming

Goal: a request crosses the gateway and is accounted for exactly.

- [ ] `cmd/fakeprovider` standalone binary
- [ ] Scenarios controlled by header: latency, chunk rate, mid-stream failure, different served model
- [ ] `httptest` in-process harness sharing the same core
- [ ] `provider.Provider` interface (extension point for later failover)
- [ ] OpenAI adapter, non-streaming
- [ ] Anthropic adapter, non-streaming, dialect translation
- [ ] Google adapter, non-streaming, dialect translation
- [ ] Model to provider routing table, optional explicit prefix override
- [ ] Tool calling passthrough
- [ ] `POST /v1/chat/completions` non-streaming, full path
- [ ] `GET /v1/models` from the local price snapshot
- [ ] Error envelope in OpenAI format, full status table per spec §7.4
- [ ] `provider_request_id` captured, including on failure
- [ ] Usage records written synchronously for now (async arrives in F7)
- [ ] TEST: end-to-end happy path, exact accounting
- [ ] TEST: each error in the table returns its stated code
- [ ] TEST: unpriced + budget = 400; unpriced without budget = 200 with NULL cost
- [ ] TEST: served_model differs from requested — priced on served, metric fires
- [ ] TEST: tool calling round-trip
- [ ] REVIEW CHECKPOINT — wait for approval

## F6 — Streaming

Goal: chunks forwarded with zero buffering, counted in flight, always settled.

- [ ] SSE parser, frame-level, 1 MB cap
- [ ] Byte-for-byte forwarding, no re-serialisation on the OpenAI path
- [ ] Anthropic and Google SSE translation to the OpenAI dialect
- [ ] Explicit Flush per event; fail loudly when http.Flusher is absent
- [ ] Compression disabled on the streaming path
- [ ] `X-Accel-Buffering: no`, `Cache-Control: no-cache`
- [ ] Per-write deadline via http.ResponseController (no global WriteTimeout)
- [ ] Provider context: WithoutCancel + own timeout + own cancel func
- [ ] `disconnect_policy: cancel` (default) invokes cancel on write failure
- [ ] `disconnect_policy: drain` with its own timeout
- [ ] Drain semaphore, default 64, configurable
- [ ] Pump clears the writer reference once clientGone; read-only thereafter
- [ ] Settle exactly once, in defer, on every path
- [ ] `stream_options.include_usage` injected; final chunk stripped if unrequested
- [ ] In-flight counting; tiktoken at settle only
- [ ] `usage_source` recorded: provider / tokenizer / estimate
- [ ] `ttft_ms` recorded
- [ ] SSE error event when a stream fails after 200 headers
- [ ] TEST: happy stream, exact usage, `usage_source=provider`
- [ ] TEST: client disconnects, policy cancel — upstream cancelled, flag set
- [ ] TEST: client disconnects, policy drain — exact usage, semaphore respected
- [ ] TEST: provider timeout mid-stream
- [ ] TEST: malformed chunk forwarded anyway, parse_errors > 0
- [ ] TEST: unterminated frame beyond 1 MB, memory bounded
- [ ] TEST: provider closes without [DONE]
- [ ] TEST: error after 200 headers — SSE error event, status 200 + error_code
- [ ] TEST: real socket close mid-stream (not a mock)
- [ ] TEST: verify per provider that closing the connection halts generation
- [ ] FUZZ: SSE parser, corpus seeded from fake provider scenarios
- [ ] CI: fuzz job, 30s budget; nightly longer
- [ ] BENCH: `BenchmarkStreamOverhead` (TTFT and per-chunk)
- [ ] REVIEW CHECKPOINT — wait for approval

## F7 — Usage pipeline, read API, observability

Goal: spend is queryable, and secrets never leak.

- [ ] Async usage buffer + batch flush
- [ ] Graceful shutdown: stop accepting, await settles, flush buffer, exit
- [ ] `GET /v1/usage` with group_by whitelist, max two dimensions
- [ ] Mandatory time range, configurable max width (default 90 days)
- [ ] `GET /v1/usage/series` by day or hour
- [ ] `GET /v1/usage/requests`, keyset pagination on (created_at, id)
- [ ] `GET /v1/budget` for the calling key
- [ ] `COSTLANE_LOG_PROMPTS` writing to `request_payloads`
- [ ] Structured logging with redaction; virtual keys as prefix only
- [ ] Provider keys never logged, never returned, never in an error message
- [ ] `/metrics` on a separate address, low-cardinality labels
- [ ] All metrics from spec §7.6
- [ ] `/healthz` (liveness) and `/readyz` (DB + price snapshot loaded)
- [ ] Retention job, configurable
- [ ] TEST: graceful shutdown completes an in-flight settle and flushes
- [ ] TEST: group_by rejects anything outside the whitelist
- [ ] TEST: a range wider than the maximum is refused
- [ ] TEST: keyset pagination is stable across inserts
- [ ] TEST: `/readyz` fails when prices are not loaded
- [ ] LEAK TEST: synthetic keys planted in env and in fake error bodies
- [ ] LEAK TEST: every error path exercised; fails if the pattern surfaces
- [ ] CI: leak job
- [ ] REVIEW CHECKPOINT — wait for approval

## F8 — Benchmarks, docs, release

Goal: the numbers are published and the first command works for free.

- [ ] `BenchmarkProxyOverhead` (non-streaming)
- [ ] benchstat comparison posted to the PR
- [ ] CI benchmark gate at >100% p99 regression only
- [ ] Measurement run on a dedicated machine, hardware stated
- [ ] p50/p99 figures in the README, with methodology
- [ ] `docker-compose.yml`: gateway + Postgres + fakeprovider
- [ ] VERIFY: `docker compose up` yields a working gateway with no real keys
- [ ] README: quickstart, configuration, API, cost model
- [ ] ADR set for the load-bearing decisions
- [ ] CONTRIBUTING, SECURITY.md
- [ ] Full security review pass
- [ ] VERIFY: every box above is ticked
- [ ] REVIEW CHECKPOINT — v1 complete

---

## Parallelism

Independent work suitable for separate worktrees:

- F2 (pricing) and F3 (auth) are independent once F1 lands
- F5's fake provider can be built alongside F4
- Security review and leak tests run as a parallel track from F3 onward
- Critical review of each phase runs in parallel with the next phase's tests

## Cross-cutting invariants

Checked at every review checkpoint:

- [ ] No provider key in any log, response, or error message
- [ ] Every money column named `_usd` and typed NUMERIC
- [ ] Every window computation uses `AT TIME ZONE 'UTC'`
- [ ] Reserve, settle, and reap are each exactly one transaction
- [ ] Test written before implementation
