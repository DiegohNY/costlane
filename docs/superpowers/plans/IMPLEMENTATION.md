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
- [x] Package skeleton per spec §3, each documented (config's package doc lives in config.go)
- [x] `internal/config`: env to struct, no defaults hidden in code
- [x] Boot validation: `drain_timeout <= provider_timeout`; `TTL >= provider_timeout + drain_timeout` (margin applies to the default only)
- [x] TEST: config validation rejects each invalid combination
- [x] `.golangci.yml` with errcheck, bodyclose, gosec, staticcheck
- [x] CI: lint job
- [x] CI: `go test ./... -race`
- [x] CI: build + docker build
- [x] `Dockerfile`, distroless base, non-root user
- [x] README skeleton with the agreed tagline
- [x] `.gitignore` (Go, .env*, *.pem, .DS_Store) + manual secret scan of history
- [x] Public repo `DiegohNY/costlane` created and pushed
- [x] Branch protection on main (PR required, no force push, no deletion)
- [x] Attach CI jobs as required status checks (lint, test, build; strict)
- [x] `enforce_admins: true` — protection applies to the repository owner too
- [x] VERIFY: protection read back from the API and compared field by field
- [x] VERIFY: CI red on a real bug (.gitignore excluded `cmd/`) blocked the merge
- [x] REVIEW CHECKPOINT — approved

### F0 retrospective

Two mistakes, both corrected:

**`.gitignore` excluded the command packages.** The binary patterns
`costlane` and `fakeprovider` had no leading slash, so git applied them at
every level and swallowed `cmd/costlane/` and `cmd/fakeprovider/`. The code
compiled locally from the working copy and never entered the commit. Only
the docker build, which sees tracked files alone, caught it. Fixed by
anchoring both patterns to the root, plus a CI step that fails when any Go
source is untracked — so the error now names its own cause.

**A pinned linter version broke the job it was meant to stabilise.**
golangci-lint v2.6.1 is built with Go 1.25 and refuses a config targeting
1.27. The pin is right; the version was picked without checking. Now on
v2.13.2, and the lesson is that a pin must be verified against the
toolchain, not assumed.

**Branch protection was tested destructively, and was bypassable.**
`enforce_admins` was left false, making the rules decorative for the only
contributor, and the check was performed by pushing a throwaway commit to
`main`. It went through as a bypassed rule violation. Protection was
recreated with `enforce_admins: true` and is now verified by reading the
API and comparing field by field.

## F1 — Storage, migrations, schema

Goal: the schema exists and its invariants are enforced by Postgres.

- [x] goose as a library, `.sql` files via `embed.FS`
- [x] Advisory lock around migration application
- [x] `COSTLANE_MIGRATE_ON_BOOT` flag (default true), wired in main and tested
- [x] Migration: `CREATE EXTENSION btree_gist` (annotated; may need privileges)
- [x] Migration: `virtual_keys` per spec §7.3
- [x] Migration: `key_budgets` per spec §5.2
- [x] Migration: `budget_reservations` + partial index on pending
- [x] Migration: `model_prices` + EXCLUDE constraint per spec §6.1
- [x] Migration: `model_aliases` + EXCLUDE constraint per spec §6.2
- [x] Migration: `usage_records`, BRIN on created_at, btree on (key_id, created_at)
- [x] Migration: `request_payloads` (separate table, droppable)
- [x] Separate read/write pools; `statement_timeout` on the read pool
- [x] testcontainers harness with automatic skip when Docker is absent
- [x] TEST: migrations apply cleanly from empty, and are idempotent
- [x] TEST: overlapping price rows rejected by EXCLUDE
- [x] TEST: overlapping alias rows rejected by EXCLUDE
- [x] TEST: two concurrent migrators, advisory lock holds
- [x] CI: integration job (testcontainers)
- [x] CHECK constraints on every money column (reserved, spent, estimated, actual, limit)
- [x] `state` as text + CHECK, not a Postgres enum
- [x] Migrations split by domain, 0001-0005, one transaction each
- [x] TEST: every down migration runs, then up again
- [x] TEST: negative constraint tests — overlapping price, overlapping alias,
      negative reservation, invented state, inverted range, negative rate
- [x] Postgres pinned (17-alpine), never latest
- [x] TEST: btree_gist installs without superuser (trusted extension since PG13)
- [ ] REVIEW CHECKPOINT — wait for approval

## F2 — Pricing and aliases

Goal: given a model and a token count, the exact cost — or an explicit refusal.

- [x] YAML schema for prices and aliases
- [x] `pricing/*.yaml` seeded for OpenAI, Anthropic, Google, with source_url
- [x] Prices load from embedded YAML into an in-memory snapshot; the
      Postgres mirror is deferred, since nothing reads prices from the
      database and a second copy could disagree with the one in use
- [x] In-memory snapshot via atomic.Pointer, replaced wholesale
- [x] `POST /admin/pricing/reload`
- [x] Exact model matching — no startswith, no substring
- [x] Alias resolution, historised, exact at each step
- [x] Context-tier selection (input_tokens_from / _to)
- [x] Four-way cost arithmetic (input, cached_read, cache_write, output)
- [x] OpenAI caching normalisation: subtract cached_tokens from prompt_tokens
- [x] Anthropic caching normalisation: separate cache_read / cache_creation
- [x] Fail-closed: missing row or null rate means unknown, never free
- [x] TEST: cost arithmetic across all four token classes
- [x] TEST: `gpt-4` must not match `gpt-4o`
- [x] TEST: alias resolving to different snapshots at different dates
- [x] TEST: tier boundary, exactly at the edge
- [x] TEST: caching normalisation reconciles with each provider's total
- [x] TEST: unknown model yields unknown, never zero cost
- [x] Long-format price rows: one per (model, provider, kind, tier, range, window)
- [x] `service_tier` in the key and the EXCLUDE; seed is standard only
- [x] Google normalisation: subtract cached, ADD thoughts (the only two-way provider)
- [x] Anthropic cache-write TTL split (5m and 1h are priced 1.6x apart)
- [x] `partially_priced`: charge the priced kinds, name the unpriced ones
- [x] Tier guard on reserve (`COSTLANE_TIER_GUARD`, default 0.75)
- [x] Decimal throughout; property test proves no drift over 2000 combinations
- [x] Reload is all-or-nothing; a broken file leaves the old table serving
- [x] TEST: every seeded rate asserted against the figure read from the provider page
- [x] TEST: concurrent reads during reload never see a half-applied table
- [ ] CI: weekly pricing source verification, opens an issue, never fails the build
- [ ] CI: unreachable page is reported as unverifiable, not as verified
- [ ] REVIEW CHECKPOINT — wait for approval

## F3 — Auth and virtual keys

Goal: a key authenticates in one round-trip and revocation takes effect at once.

- [x] Key generation `cl_<32 bytes base62>`, crypto/rand
- [x] SHA-256 hashing, unique index, `key_prefix` for display
- [x] Master key from env, constant-time comparison
- [x] `POST /admin/keys` — secret returned exactly once
- [x] `GET /admin/keys` — never returns secrets
- [x] `PATCH /admin/keys/{id}` — budget, allowed_models, disconnect_policy
- [x] PATCH validates `drain_timeout <= provider_timeout` per key
- [x] `DELETE /admin/keys/{id}` — soft revoke
- [x] Scope type: repository forces `WHERE key_id` for a virtual key
- [x] TEST: secret is not retrievable after creation
- [x] TEST: revoked key visible immediately, no cache window (keys are never cached)
- [x] TEST: a virtual key cannot read another key's usage
- [x] TEST: scope parameter is non-optional (compile-time)
- [x] `obs.Secret` type: redacted in %v, %q, %#v, JSON and slog; Expose() to read
- [x] TEST: no rendering of Config leaks the master key or the DB password
- [x] TEST: 10,000 generated keys are unique, well-formed and unbiased
- [x] Credentials in the query string rejected with 400, never logged or echoed
- [x] Key creation is one transaction: virtual_keys + key_budgets
- [x] PATCH distinguishes absent, null and empty for every nullable field
- [x] DELETE is idempotent (204 on repeat, not 404)
- [x] metadata is a flat string map, capped at 4 KB, validated on the way in
- [x] Test harness moved to `internal/storetest` so testcontainers stays out
      of the production binary
- [ ] REVIEW CHECKPOINT — wait for approval

## F4 — Budget: reserve, settle, reaper

Goal: a thousand concurrent requests never breach one counter.

- [x] Fused reserve statement: auth + allowed_models + rotation + reserve
- [x] Lazy UTC month rotation inside the reserve
- [x] `limit_usd IS NULL` means unlimited
- [x] Reservation INSERT in the same transaction, window from RETURNING
- [x] Diagnostic SELECT on zero rows: classify 401 / 402 / 403
- [x] Cheap reserve estimate: chars/4 + max_tokens (or 4096 default)
- [x] Settle: one transaction, state always terminal
- [x] Settle reads old state via `settled_after_expiry` in SET, not RETURNING state
- [x] Settle releases reserved conditionally (no double release after expiry)
- [x] Settle adds to spent only when window matches
- [x] Overshoot flag + metric; no `max_tokens` injection
- [x] Reaper: single CTE, state change and release together
- [x] Reaper uses FOR UPDATE SKIP LOCKED, safe for N replicas
- [x] Three reconciliation queries as functions (the admin endpoint lands in F7)
- [x] TEST: N concurrent reserves on one key; spent+reserved <= limit; exact success count
- [x] TEST: double settle counts once
- [x] TEST: settle after expiry — no double release, flag set
- [x] TEST: retried settle after crash does not double-count
- [x] TEST: reaper versus settle race, exactly one wins
- [x] TEST: reservation spanning month boundary lands in its own window
- [x] TEST: unlimited key (NULL limit) is never refused
- [x] TEST: all three reconciliations return zero after concurrent load
- [x] READ COMMITTED set explicitly on all three transactions, asserted by test
- [x] Settle runs on a detached context, so a disconnected client cannot abort it
- [x] Reaper locks budget rows in key order, retries on deadlock (SQLSTATE 40P01)
- [x] TEST: two reapers over 50 keys for 20 iterations, no unhandled deadlock
- [x] TEST: deliberately broken reserve/settle make the concurrency tests fail
- [x] Reaper goroutine with jittered interval, finishes its batch on SIGTERM
- [x] 402 body carries limit, spent, reserved and window_resets_at
- [x] BENCH: `BenchmarkReserveContention` (0.72ms contended, 0.28ms distinct keys)
- [ ] REVIEW CHECKPOINT — wait for approval

## F5 — Fake provider and proxy, non-streaming

Goal: a request crosses the gateway and is accounted for exactly.

- [x] `cmd/fakeprovider` standalone binary
- [x] Scenarios controlled by header: latency, chunk rate, mid-stream failure, different served model
- [x] `httptest` in-process harness sharing the same core
- [x] `provider.Provider` interface (extension point for later failover)
- [x] OpenAI adapter, non-streaming
- [x] Anthropic adapter, non-streaming, dialect translation
- [x] Google adapter, non-streaming, dialect translation
- [x] Model to provider routing table, optional explicit prefix override
- [x] Tool calling passthrough
- [x] `POST /v1/chat/completions` non-streaming, full path
- [x] `GET /v1/models` from the local price snapshot
- [x] Error envelope in OpenAI format, full status table per spec §7.4
- [x] `provider_request_id` captured, including on failure
- [x] Usage records written synchronously for now (async arrives in F7)
- [x] TEST: end-to-end happy path, exact accounting
- [x] TEST: each error in the table returns its stated code
- [x] TEST: unpriced + budget = 400; unpriced without budget = 200 with NULL cost
- [x] Fixed: an unpriced model estimates at zero, so the budget check had to
      move ahead of the reserve — a zero reservation succeeds against any limit
- [x] TEST: served_model differs from requested — priced on served, metric fires
- [x] TEST: tool calling round-trip
- [x] Fake provider speaks all three native dialects, verified against the normalisers
- [x] Byte-for-byte passthrough for OpenAI; surgical edits via json.RawMessage
- [x] TEST: a body with ten invented fields reaches the provider unchanged
- [x] Fail-closed translation: an unsupported parameter is refused by name
- [x] Golden fixtures pin the Anthropic translation, one file per case
- [x] max_tokens injected only where a provider requires it, declared in a header
- [x] Upstream error bodies pass through redaction; Retry-After preserved
- [x] Dedicated HTTP client with keep-alive and HTTP/2, never DefaultClient
- [x] MaxBytesReader on the body; X-Costlane-Request-Id on every response
- [x] X-Costlane-Cost-Usd and X-Costlane-Budget-Remaining-Usd on non-stream replies
- [x] Validation happens before the reserve, so no wasted round trip or audit row
- [x] VERIFY: real gateway against real Postgres and the fake provider
- [ ] REVIEW CHECKPOINT — wait for approval

## F6 — Streaming

Goal: chunks forwarded with zero buffering, counted in flight, always settled.

- [x] SSE parser, frame-level, 1 MB cap
- [x] Byte-for-byte forwarding, no re-serialisation on the OpenAI path
- [x] Anthropic and Google SSE translation to the OpenAI dialect
- [x] Explicit Flush per event; fail loudly when http.Flusher is absent
- [x] Compression disabled on the streaming path
- [x] `X-Accel-Buffering: no`, `Cache-Control: no-cache`
- [x] Per-write deadline via http.ResponseController (no global WriteTimeout)
- [x] Provider context: WithoutCancel + own timeout + own cancel func
- [x] `disconnect_policy: cancel` (default) invokes cancel on write failure
- [x] `disconnect_policy: drain` with its own timeout
- [x] Drain semaphore, default 64, configurable
- [x] Pump clears the writer reference once clientGone; read-only thereafter
- [x] Settle exactly once, in defer, on every path
- [x] `stream_options.include_usage` injected; final chunk stripped if unrequested
- [x] In-flight counting; tiktoken at settle only
- [x] `usage_source` recorded: provider / tokenizer / estimate
- [x] TTFT recorded in microseconds (milliseconds rounded a fast path to zero)
- [x] SSE error event when a stream fails after 200 headers
- [x] TEST: happy stream, exact usage, `usage_source=provider`
- [x] TEST: client disconnects, policy cancel — upstream cancelled, flag set
- [x] TEST: client disconnects, policy drain — exact usage, semaphore respected
- [x] TEST: provider timeout mid-stream
- [x] TEST: malformed chunk forwarded anyway, parse_errors > 0
- [x] TEST: unterminated frame beyond 1 MB, memory bounded
- [x] TEST: provider closes without [DONE]
- [x] TEST: error after 200 headers — SSE error event, status 200 + error_code
- [x] TEST: real socket close mid-stream (not a mock)
- [ ] TEST: verify per provider that closing the connection halts generation
      — DEFERRED: needs real credentials against each provider. The cancel
      policy rests on the documented behaviour that providers stop generating
      on disconnect; until that is confirmed against live APIs, an operator who
      wants certainty over cost can set disconnect_policy to drain.
- [x] FUZZ: SSE parser, corpus seeded from fake provider scenarios
- [x] CI: fuzz job, 30s budget; nightly 10m on both targets
- [x] Streaming tool calling for translated providers, with golden stream fixtures
- [x] Disconnect detected from the request context between chunks, not only
      from a failed write
- [x] Per-write deadline treats a client that stops reading as gone
- [x] Every ResponseWriter wrapper implements Unwrap, with a test that mounts
      the production middleware chain and proves chunks still arrive separately
- [x] Usage chunk stripped by shape, never by position
- [x] FUZZ: the Anthropic stream translator, 2.9M executions clean
- [x] Stream metrics: chunks, TTFT, disconnects by policy, drains in flight,
      semaphore wait
- [x] Fixed: OpenAI streaming counted no tokens, because the usage chunk was
      stripped before anything read it
- [x] Fixed: stream accounting ran on the cancelled request context, so a
      disconnected client left no usage record at all
- [x] BENCH: stream pump ~1.1us per chunk, frame reader 271 MB/s
- [ ] REVIEW CHECKPOINT — wait for approval

## F7 — Usage pipeline, read API, observability

Goal: spend is queryable, and secrets never leak.

- [x] Async usage buffer + batch flush
- [x] Graceful shutdown: stop accepting, await settles, flush buffer, exit
- [x] `GET /v1/usage` with group_by whitelist, max two dimensions
- [x] Mandatory time range, configurable max width (default 90 days)
- [x] Day and hour grouping on UTC boundaries (folded into /v1/usage)
- [x] `GET /v1/usage/requests`, keyset pagination on (created_at, id)
- [x] `GET /v1/budget` for the calling key
- [x] `COSTLANE_LOG_PROMPTS` writing to `request_payloads`
- [x] Structured logging with redaction; virtual keys as prefix only
- [x] Provider keys never logged, never returned, never in an error message
- [x] `/metrics` on a separate address, low-cardinality labels
- [x] All metrics from spec §7.6
- [x] `/healthz` (liveness) and `/readyz` (DB + price snapshot loaded)
- [x] Retention job, configurable
- [x] TEST: graceful shutdown completes an in-flight settle and flushes
- [x] TEST: group_by rejects anything outside the whitelist
- [x] TEST: a range wider than the maximum is refused
- [x] TEST: keyset pagination is stable across inserts
- [x] TEST: `/readyz` fails when prices are not loaded
- [x] LEAK TEST: synthetic keys planted in env and in fake error bodies
- [x] LEAK TEST: every error path exercised; fails if the pattern surfaces
- [x] Bounded buffer with synchronous fallback, never a drop
- [x] TEST: 300 records survive a three-second database outage
- [x] Batch writes via COPY, all-or-nothing
- [x] Readiness fails before the listener stops, so a load balancer stops first
- [x] Separate retention for usage records and request payloads
- [x] Fixed: a nil token detail marshalled to JSON null and violated the
      object constraint, which would have broken any record with no tokens
- [x] Found by the leak test: redaction only knew the credential shapes
      providers use today, so an unrecognised one passed straight through
- [x] CI: leak job
- [x] TEST: with prompt logging on, a prompt appears only in request_payloads
- [x] Redactor knows this process's own credentials, not only known formats
- [ ] REVIEW CHECKPOINT — wait for approval

## F8 — Benchmarks, docs, release

Goal: the numbers are published and the first command works for free.

- [x] `BenchmarkNonStreamGateway` / `BenchmarkNonStreamDirect` (non-streaming
      overhead, measured as a delta rather than in isolation)
- [x] `BenchmarkStreamGateway` / `BenchmarkStreamDirect` (TTFT and per-chunk)
- [x] benchstat comparison posted to the PR step summary
- [x] CI benchmark gate at >100% regression only
- [x] Measurement run on stated hardware, by a CI job anyone can read
- [x] p50/p99 figures in the README, with methodology
- [x] `docker-compose.yml`: gateway + Postgres + fakeprovider
- [x] VERIFY: `docker compose up` yields a working gateway with no real keys,
      checked by the `quickstart` CI job running the README's own commands
- [x] README: quickstart, configuration, API, cost model
- [x] ADR set for the decisions that reversed an earlier one
- [x] SECURITY.md with a threat model and a reporting channel
- [x] `govulncheck` as a blocking CI job
- [x] The server refuses to boot without a master key, or with prompt logging
      and no retention
- [x] A test that counts the database round trips on the happy path
- [x] Live-provider verification harness (`internal/providerverify`)
- [x] Live-provider verification RUN as far as credentials allowed, with
      results and non-results in `docs/provider-verification.md`: Gemini
      non-streaming counts verified exactly; OpenAI and Anthropic not
      verified (no credential); cancellation not verifiable on Google (no
      streaming adapter) and not verified elsewhere
- [x] Version stamped by ldflags and reported by `/healthz`
- [x] Multi-arch image published to GHCR on a tag
- [x] v0.1.0 tagged, with a hand-written changelog
- [x] VERIFY: multi-arch image present on GHCR for linux/amd64 and
      linux/arm64, confirmed by reading the registry manifest
- [x] VERIFY: every box above is ticked, or carries the reason it is not
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
- [ ] No test commit on `main`, ever — verify protection by reading the API,
      and any behavioural test uses a throwaway canary branch

---

## Where this stands

Last updated 2026-09-10, at v0.1.0.

**Merged through F8, and released.** The gateway proxies, meters and enforces
budgets, with streaming, a read API, health probes and a leak test. CI runs
nine required jobs (lint, test, integration, fuzz, leak, build, quickstart,
vulnerabilities, bench).

**F8 is complete.** v0.1.0 is tagged and the image is published.

### Picking this up on another machine

```bash
git clone https://github.com/DiegohNY/costlane
cd costlane
go test ./... -race          # needs Docker for the integration packages
```

The integration, leak and benchmark tests start a pinned Postgres through
testcontainers, so Docker has to be running; without it they skip rather than
fail. `golangci-lint` is pinned to v2.13.2 in CI, and running it locally
before pushing saves a round trip — three of the first pull requests were
turned red by something it finds in seconds.

### Where v0.1.0 landed

Tagged 2026-09-10. Nine required CI jobs: lint, test, integration, leak,
fuzz, build, quickstart, vulnerabilities, bench. The image is published to
`ghcr.io/diegohny/costlane:v0.1.0` for amd64 and arm64, with the version
stamped by ldflags and reported by `/healthz`.

Three things found while closing the phase, none of them by review:

- **`govulncheck` found three reachable vulnerabilities on its first run**
  (two in `golang.org/x/crypto`, one in `github.com/moby/go-archive`). Both
  modules upgraded.
- **The provider verification passed while verifying nothing.** It compared
  costlane's counts against a body costlane had itself translated. It now
  goes through a recording proxy and compares against the provider's own
  bytes. See docs/provider-verification.md.
- **The release workflow would have failed on the tag**, because GHCR
  rejects a capital letter in an image name and this repository has two.
  Found by reading the workflow before tagging rather than by tagging.

### What v0.1.0 does not establish

**The cancel default is unmeasured on every provider.** OpenAI and Anthropic
had no funded credential; Google has no streaming adapter, so it has no
stream to cancel. The README's known limitations lead with this, and name
`disconnect_policy: drain` as the remedy for anyone who needs certainty
rather than a documented default. Issues #12 and #13.

**Prices are not mirrored into Postgres** (F2). They load from embedded YAML
into an in-memory snapshot, which is what every lookup reads. A second copy
in the database would be a second source of truth that could disagree with
the one in use, so it was left out until something actually needs it.

### v0.2

Three issues, labelled `v0.2`:

- #12 — Gemini streaming adapter. Also unblocks the cancellation check.
- #13 — Verify cancel stops billing on real providers, by the method in
  docs/provider-verification.md. Needs funded credentials.
- #14 — Take the budget-remaining figure from the settle's `RETURNING`
  rather than a separate `SELECT`, removing a round trip from the happy
  path.
