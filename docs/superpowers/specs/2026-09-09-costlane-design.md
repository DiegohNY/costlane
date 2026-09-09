# costlane — Design Document

**Status:** approved
**Date:** 2026-09-09
**Module:** `github.com/DiegohNY/costlane`
**License:** Apache-2.0

> costlane — an LLM gateway that meters tokens, costs and budgets per key.
> Drop-in OpenAI-compatible.

---

## 1. Purpose and scope

costlane is a reverse proxy that sits between applications and LLM providers
(OpenAI, Anthropic, Google). It speaks the OpenAI API dialect, so a client
switches to it by changing `base_url` and keeps its existing SDK.

Version 1 does five things and nothing else:

1. Proxies chat completions, including SSE streaming, with minimal overhead.
2. Accounts for tokens and cost exactly, on every request.
3. Issues virtual keys, one per team, feature, or customer, from which spend
   attribution follows.
4. Enforces per-key budgets with a hard stop that stays correct under
   concurrency.
5. Exposes a read API for querying spend and usage.

Explicitly out of scope for v1: semantic caching, multi-provider failover,
evaluation suites, and any frontend. The design leaves room for each of them,
but none is implemented.

### Design stance

Two commitments shape every decision below.

The first is that the gateway sits **in the request path**. This is what makes
a hard stop possible at all: a system that observes spend after the fact can
report and alert, but it cannot refuse. Section 2 examines three comparable
open-source projects, and none of them occupies this position.

The second is that **the numbers must be exact**. Token accounting is not a
telemetry nicety here; it is the product. Where exactness is impossible — a
stream cut short before the provider reports usage — the record says so
explicitly rather than presenting an estimate as a measurement.

---

## 2. Competitive positioning

Three open-source projects occupy adjacent ground. The first axis of
comparison is whether a system sits in the request path or observes after the
fact, because that single property determines whether it can enforce anything.

### 2.1 AlameerAshraf/tokentoll (77 stars)

Despite the name, this is not a gateway. It positions itself as an open-source
alternative to Lago and Orb — usage-based billing infrastructure. The
application calls the provider directly and then reports a usage event to
TokenToll over HTTP. It is a downstream layer, not a competitor, and in
principle costlane could feed it.

Its architecture is FastAPI, MongoDB, and Redis Streams, with a React
dashboard and unpublished Node and Python SDKs. Pricing lives in twelve
per-provider YAML files.

What it does not do is instructive. There is no streaming support of any kind;
because TokenToll never sees the model's response, counting tokens mid-stream
is the caller's problem. Its budget check is not a hard stop: ingestion returns
`202 Accepted` and enqueues, and limit enforcement runs in a worker after the
provider call has already happened. An over-budget event is recorded as
rejected, but the money is gone.

Concurrency is unhandled. The wallet update is a read-modify-write in Python
with no atomic increment, no optimistic locking, and no version column, so two
workers processing the same customer lose updates.

The costliest flaw is in the pricing math. A single scalar quantity is billed
at the arithmetic mean of the input and output rates. For a model priced at 15
in and 75 out, the result is wrong unless the true input-to-output ratio is
exactly 1:1 — and the event schema has no separate input and output token
fields at all. Prompt caching is absent entirely. The pricing table is not
versioned: its `updated_at` field is decorative and never read, so repricing a
model silently rewrites the cost of all historical usage.

Its traction signal deserves scepticism: 77 stars against 11 commits by one
author over four days, generated from a Spec Kit template, with no releases and
no activity since March 2026.

### 2.2 cassandra-web/tokengauge

A pure Python log parser with no network code. It reads the JSONL files written
by Claude Code, Cursor, and a third agent, aggregates by day, and flags
anomalies. Budgets produce alerts at 50/75/90/100 percent thresholds and
nothing more; by the time it parses a log, the tokens are spent. Pricing is
three hardcoded per-agent tuples, not per-model, with no dates and no sources.
There is no storage — state lives in a dictionary and does not survive the
process.

The repository has zero stars and a single commit, pushed 57 seconds after
creation, with no activity since. It is not a live project.

### 2.3 fablgen-agent/tokengauge

A Next.js and SQLite web application with Stripe checkout and three paid tiers.
It offers a pricing directory with calculators, browser-local audit tools that
operate on manually entered totals, and an A/B benchmark lab that sends a task
twice and compares. The lab is the only server-side outbound call, and it is a
benchmark harness — an application cannot be pointed at it.

It enforces nothing. `calculateAgentBudget()` is arithmetic, and its own UI
describes the output as an allocation plan rather than a spending cap.

One detail is worth recording. The project sells a fixed-price consulting
service that offers to add a reserve-reconcile-refuse gate to the customer's
own codebase — precisely the primitive costlane implements. The service page
concedes its own limits: no absolute cap when unbounded input, tools, parallel
calls, or an unshared store are involved. A product outside the request path
cannot enforce a cap, so it sells the work of building one. This validates the
demand and confirms the structural argument.

Its pricing table is the strongest work among the three and worth learning
from: 58 dated cards across nine providers, each with a source URL, separate
input, cached-read, cache-write, and output rates, context-tier and region
variants, and a CI job that fetches each provider's live pricing page and
asserts every rate still appears. Its semantics are fail-closed — a missing
card or a null rate means unknown, never free. costlane adopts both the
fail-closed rule and the source-verification job.

Both `tokengauge` repositories show strong signs of automated generation, so
stars and commit velocity are weak evidence of adoption.

### 2.4 Where costlane differs

| | tokentoll | cassandra | fablgen | **costlane** |
|---|---|---|---|---|
| **In request path** | no | no | no | **yes** |
| Enforcement | post-hoc | alerts only | none (sold as a service) | **hard stop before the call** |
| Streaming | absent | n/a | not incremental | **SSE forwarded, counted in flight** |
| Attribution | per customer | none | browser-local labels | **per virtual key** |
| Token model | one scalar, rates averaged | per-agent tuples | four-way split | **four-way split** |
| Pricing history | overwritten | hardcoded | dated, source-verified | **versioned, immutable** |
| Concurrency | lost updates | n/a | n/a | **atomic conditional update** |

The gap is structural rather than accidental. None of the three enforces a
spending cap, and none can: enforcement requires occupying the request path.

---

## 3. Architecture overview

```
client ──► [auth + budget reserve: ONE transaction] ──► [provider call]
                      │                                       │
                      │ 0 rows → diagnostic query → 401/402/403
                      │                                       ▼
                      │                              [SSE pump: forward,
                      │                               count in flight]
                      │                                       │
                      └────────────► [settle: ONE transaction] ◄┘
                                              │
                                     [usage record → async buffer → batch flush]
```

Packages:

```
cmd/costlane/          main, wiring, graceful shutdown
cmd/fakeprovider/      standalone SSE fake, for demos and benchmarks
internal/
  config/              env to struct, boot-time validation
  auth/                hashing, verification, scope
  budget/              reserve, settle, reaper, reconciliation
  pricing/             YAML loading, in-memory snapshot, aliases, tiers
  usage/               records, async buffer, batch flush
  provider/            interface plus openai/, anthropic/, google/
  proxy/               handler, SSE pump, dialect translation
  api/                 read and admin routes
  store/               Postgres, migrations, separate read/write pools
  obs/                 structured logging with redaction, metrics
```

The `provider.Provider` interface is the extension point for the
multi-provider failover that v1 excludes. A router that today maps a model to
one provider can tomorrow try several, without touching the pump.

---

## 4. Streaming

Streaming is where overhead and accuracy are both decided.

### 4.1 Disconnection policy

When a client disconnects mid-stream, the default is to **cancel** the upstream
call, not to drain it. Providers stop generating when the connection closes and
bill only for tokens already produced. Draining to `[DONE]` in order to learn an
exact number means paying for tokens nobody will read — for a cost-control
product, the wrong trade.

The in-flight counter makes cancellation accountable: the settle records
`usage_source = tokenizer` (or `estimate`) and sets `client_disconnected`.

Draining remains available per key via `disconnect_policy: cancel | drain`, for
operators who want exact accounting at the price of wasted tokens. Whether
closing the connection actually halts generation must be verified per provider.

### 4.2 Context and cancellation

The upstream call uses a context derived with `context.WithoutCancel` from the
request context, so a client disconnect does not implicitly kill it, plus its
own timeout and its own cancel function. Under the `cancel` policy, a write
failure to the client invokes that cancel function explicitly.

Concurrent drains are bounded by a semaphore (default 64, configurable). A
client that opens and abandons a thousand requests in a second must not turn
into a thousand upstream streams; the key's budget bounds this economically,
but goroutines and connections are our resource.

The pump must not touch the `ResponseWriter` after the handler returns. Once
`clientGone` is set, the writer reference is cleared and the loop only reads.
The settle happens exactly once, in a `defer`, on every path.

### 4.3 Forwarding

A single pump goroutine reads a frame, updates the counter, and writes to the
client in the same iteration. There is no intermediate channel, because a
channel is a buffer and the requirement is zero buffering.

Every SSE event is followed by an explicit `Flush()`. At setup the writer is
checked for `http.Flusher` and the request fails loudly if it is absent, rather
than degrading silently. Compression is disabled on the streaming path, since an
interposed gzip writer reintroduces buffering.

Streaming responses carry `X-Accel-Buffering: no` and `Cache-Control: no-cache`,
because any reverse proxy in front of costlane would otherwise buffer and defeat
the point.

Write deadlines are set per write via `http.ResponseController.SetWriteDeadline`.
A global `WriteTimeout` on `http.Server` would kill long streams, a failure that
short tests never reveal.

### 4.4 Parsing

Chunks are scanned at frame level and forwarded **byte for byte as received**,
without re-serialisation. Only chunks that may carry usage are deserialised.
For Anthropic and Google, whose native SSE formats differ, translation to the
OpenAI dialect requires rewriting each chunk; that cost is per-chunk and
involves no accumulation.

The frame buffer is capped at 1 MB. Beyond that, raw bytes are forwarded as-is,
`parse_errors` is marked, and the loop continues — consistent with the rule
below, without allowing memory exhaustion.

**A proxy does not censor what it cannot parse.** A malformed chunk is forwarded
to the client anyway, logged, and recorded with `parse_errors > 0`. Blocking it
would make costlane a breaking point for every future change in provider
formats. We lose counting precision and declare it.

### 4.5 Token sources

In order of precedence, recorded per request as `usage_source`:

1. **`provider`** — usage reported by the provider. For OpenAI this is obtained
   by injecting `stream_options.include_usage`, stripping the final chunk if the
   client did not ask for it.
2. **`tokenizer`** — `tiktoken` over input and accumulated output, when a stream
   ends before usage arrives.
3. **`estimate`** — chars/4 heuristic for Anthropic and Google interruptions,
   where no exact local tokenizer exists.

### 4.6 Failure matrix

| Case | Behaviour |
|---|---|
| Client disconnects, policy `cancel` | upstream cancelled, settle from in-flight count, `client_disconnected` |
| Client disconnects, policy `drain` | drained to `[DONE]` under semaphore, exact usage |
| Provider times out mid-stream | settle from fallback count, SSE error event if client still attached |
| Malformed chunk | forwarded anyway, logged, `parse_errors > 0` |
| Frame exceeds 1 MB without terminator | raw bytes forwarded, `parse_errors`, memory bounded |
| Provider closes without `[DONE]` | treated as interruption, fallback settle |
| Drain exceeds its timeout | abandoned, settle from count, `drain_timeout` flag |

---

## 5. Budgets

### 5.1 Approach

Three approaches were considered. Pessimistic row locking (`SELECT … FOR
UPDATE`) is simple and obviously correct but serialises every request for a key
onto one row. Summing an append-only log inside a `SERIALIZABLE` transaction
never diverges but grows with history and produces retry storms under exactly
the contention we must survive.

The chosen approach is a **materialised balance plus an append-only reservation
log, updated by an atomic conditional statement**. The predicate is evaluated
inside the same `UPDATE` that writes, so there is no window between check and
write, and no `SELECT` round-trip on the critical path.

Its known weakness is that the materialised balance can drift from the log if a
bug is introduced. Three reconciliation queries and a Prometheus gauge guard
against this.

### 5.2 Schema

```
key_budgets
  key_id        uuid PK
  window_start  date              -- current UTC month window
  limit_usd     numeric(20,10) NULL   -- NULL = unlimited
  spent_usd     numeric(20,10)
  reserved_usd  numeric(20,10)

budget_reservations
  id                    uuid PK
  key_id                uuid
  window_start          date          -- window of the RESERVE
  state                 text          -- pending | settled | expired
  estimated_usd         numeric(20,10)
  actual_usd            numeric(20,10) NULL
  expires_at            timestamptz
  overshoot             bool
  settled_after_expiry  bool
  created_at, settled_at

  INDEX (expires_at) WHERE state = 'pending'
```

Money is `NUMERIC` in dollars, and column names say `_usd`. A name that lies
about its unit is a future bug.

There are three states, not four. A release is a settle with `actual_usd = 0`,
which removes a transition from the test matrix. `settled` is always terminal.

All window arithmetic uses `date_trunc('month', now() AT TIME ZONE 'UTC')`.
Without an explicit zone the reset would move with the database session's
timezone configuration. **Budgets are per UTC month**, documented as such.

### 5.3 Reserve — authentication, model check, rotation, and reservation

Authentication and reservation are fused into a single statement, so the common
path costs one round-trip rather than two:

```sql
UPDATE key_budgets kb
   SET spent_usd = CASE WHEN kb.window_start < date_trunc('month', now() AT TIME ZONE 'UTC')::date
                        THEN 0 ELSE kb.spent_usd END,
       window_start = GREATEST(kb.window_start,
                               date_trunc('month', now() AT TIME ZONE 'UTC')::date),
       reserved_usd = kb.reserved_usd + $2
  FROM virtual_keys vk
 WHERE vk.key_hash = $1
   AND vk.revoked_at IS NULL
   AND (vk.allowed_models IS NULL OR $model = ANY(vk.allowed_models))
   AND kb.key_id = vk.id
   AND (kb.limit_usd IS NULL OR
        (CASE WHEN kb.window_start < date_trunc('month', now() AT TIME ZONE 'UTC')::date
              THEN 0 ELSE kb.spent_usd END) + kb.reserved_usd + $2 <= kb.limit_usd)
RETURNING kb.key_id, kb.window_start, vk.disconnect_policy, vk.drain_timeout_ms;
```

The reservation row is inserted in the same transaction, taking its
`window_start` from the `RETURNING` clause so it is atomically consistent with
the window in force at reserve time.

Zero rows means one of several things, so **and only then** a diagnostic
`SELECT` on `virtual_keys` classifies the failure as 401, 402, or 403. The
common path pays one query; the rare path pays two.

Virtual keys are never cached in memory. A cache would produce the
"revoked but still working for thirty seconds" hole, which a spend-control
product cannot have. Prices are cached, because they are versioned and
immutable.

The window rotates lazily, inside the reserve itself. A scheduled job would
race with concurrent reserves. A key with no traffic never rotates, which does
not matter: reporting reads usage records by window, not the materialised
balance.

The reserve estimate does not need to be exact: chars/4 over the input plus
`max_tokens` (or the 4096 default). `tiktoken` runs only at settle. Running a
BPE tokenizer over a 50k-token prompt before every request is overhead the
customer feels, in exchange for an estimate that the real figure replaces.

### 5.4 Settle

```sql
UPDATE budget_reservations
   SET state = 'settled',                          -- always terminal
       actual_usd = $2,
       settled_at = now(),
       overshoot = ($2 > estimated_usd),
       settled_after_expiry = (state = 'expired')  -- old value, read in SET
 WHERE id = $1 AND state IN ('pending','expired')
RETURNING estimated_usd, window_start, settled_after_expiry;

UPDATE key_budgets
   SET reserved_usd = reserved_usd
         - CASE WHEN $was_expired THEN 0 ELSE $estimated END,
       spent_usd = spent_usd
         + CASE WHEN window_start = $resv_window THEN $2 ELSE 0 END
 WHERE key_id = $3;
```

Both statements are in one transaction. Splitting them would let a crash leave
the materialised balance diverged from the log, invisible until reconciliation
runs.

Two subtleties matter.

`RETURNING` yields **new** values, so the old state cannot be read from
`RETURNING state`. The old state is captured in the `SET` clause as
`settled_after_expiry` and read from there.

The reserved amount is released **conditionally**. If the reaper already expired
the reservation, it has already subtracted; subtracting again would double-release.

Because `settled` is terminal, a retried settle after a crash matches nothing
and returns zero rows, so `actual_usd` is never added twice.

### 5.5 Overshoot

When actual exceeds estimated — no `max_tokens`, the 4096 default, a longer
response — the real amount is recorded regardless. `spent_usd` may exceed
`limit_usd` by at most one request. The row carries `overshoot`, and
`costlane_budget_overshoot_total` increments.

A hard stop means "no new request starts over budget", not "no single request
overruns". costlane does **not** inject `max_tokens` to force a ceiling: that
would change provider behaviour the client did not ask for.

Overshoot does not accumulate: because `reserved_usd` is in the predicate, the
next request sees the inflated `spent_usd` and is refused.

### 5.6 Reaper

```sql
WITH reaped AS (
  UPDATE budget_reservations
     SET state = 'expired'
   WHERE id IN (SELECT id FROM budget_reservations
                 WHERE state = 'pending' AND expires_at < now()
                 ORDER BY expires_at LIMIT 500
                 FOR UPDATE SKIP LOCKED)
  RETURNING key_id, estimated_usd
)
UPDATE key_budgets kb
   SET reserved_usd = kb.reserved_usd - r.total
  FROM (SELECT key_id, SUM(estimated_usd) AS total FROM reaped GROUP BY key_id) r
 WHERE kb.key_id = r.key_id;
```

State transition and release are one transaction, expressed as one CTE.
`SKIP LOCKED` lets N replicas run the reaper without contending. The system is
designed for N replicas from the start; v1 will run one, but anything assuming a
single process is a trap discovered the day a second one starts.

Without the reaper, a process that dies between reserve and settle leaves a
reservation pending forever and the key's budget permanently understated.

### 5.7 TTL

`TTL >= provider_timeout + drain_timeout + margin`, where `provider_timeout`
bounds the **entire stream**, not just connection setup. The rule of thumb
`TTL = 2 x provider_timeout` holds only when `drain_timeout <= provider_timeout`,
which is enforced as boot-time configuration validation and again in
`PATCH /admin/keys` for per-key overrides.

### 5.8 End-of-month eventual consistency

Spend belongs to the window in which the **reserve** happened, not the settle.
Rotation zeroes `spent_usd` but not `reserved_usd`, and pending reservations from
the previous month settle into their own window.

Consequently the previous month's total can still grow **after** the reset, for
as long as pending reservations take to close. **The bound is at most TTL
seconds**; past that, no reservation from the earlier window can still settle.

### 5.9 Reconciliation

Three queries, exposed at `GET /admin/reconcile` and asserted in integration
tests:

1. Materialised `reserved_usd` and `spent_usd` versus recomputation from the log.
2. **Settled reservations with no corresponding usage record** — the exact
   measure of what the async buffer lost in a crash. If it grows, the accepted
   risk does not hold in practice and the buffer must be made durable.
3. Sum of `cost_usd` per window versus `spent_usd` for that window.

`costlane_budget_drift_usd` exposes the first as a gauge.

---

## 6. Pricing and accounting

### 6.1 Price table

Source of truth is YAML in the repository, loaded at boot into a historised
table.

```
model_prices
  id, model, provider
  input_usd_per_mtok, cached_read_usd_per_mtok,
  cache_write_usd_per_mtok, output_usd_per_mtok   numeric(20,10)
  max_output_tokens int
  input_tokens_from bigint NOT NULL DEFAULT 0
  input_tokens_to   bigint NULL          -- NULL = unbounded
  effective_from timestamptz NOT NULL
  effective_until timestamptz NULL
  source_url text, version int

  EXCLUDE USING gist (
    model    WITH =,
    provider WITH =,
    int8range(input_tokens_from, input_tokens_to, '[)') WITH &&,
    tstzrange(effective_from, effective_until, '[)')    WITH &&)
```

The exclusion constraint makes non-overlap an invariant enforced by Postgres,
not by the loader. A YAML file that introduces an overlap fails at boot with a
database error rather than passing because the loader had a bug.

Four price columns, because prompt caching changes the price of input and
because Anthropic distinguishes cache writes from cache reads.

Context tiers are modelled in v1: Gemini above 200k tokens and Anthropic with
extended context already price by tier. The reserve picks a tier using the
chars/4 estimate — it is an estimate either way — and the settle uses real
tokens. Regions are deferred, but `provider` is in the key, so `azure-openai`
and `vertex` can arrive later as distinct providers without a migration.

Three properties the surveyed projects get wrong:

- **Exact model matching.** No `startswith`, no substring containment. `gpt-4`
  matching before `gpt-4o` is a silent bug that changes the numbers.
- **No default model.** An unknown model never falls back to another's price.
- **Immutable history.** Repricing inserts a new row with a new
  `effective_from`; existing rows are never updated. A March request keeps its
  March cost forever. Usage records store the `price_id` used, so any figure can
  be reconstructed.

Semantics are fail-closed: `effective_from` inclusive, `effective_until`
exclusive, and a missing row or a null rate means **unknown, never free**.

At runtime the per-request lookup reads an **in-memory snapshot**
(`atomic.Pointer` to an immutable struct, replaced wholesale on reload), not the
database. Otherwise every request would query for a price before even reaching
the reserve. Reload happens at boot and via `POST /admin/pricing/reload`.

### 6.2 Aliases

Providers answer to undated names — `gpt-4o`, `claude-sonnet-4-5`, `*-latest` —
that resolve to dated snapshots. With exact matching alone, a request for
`gpt-4o` on a budgeted key would be refused because only the snapshot is priced.

```
model_aliases
  alias, canonical_model, provider
  effective_from timestamptz NOT NULL
  effective_until timestamptz NULL
  PRIMARY KEY (alias, effective_from)

  EXCLUDE USING gist (alias WITH =, provider WITH =,
                      tstzrange(effective_from, effective_until, '[)') WITH &&)
```

Lookup is two exact steps: alias to canonical, canonical to price. Aliases are
historised because `gpt-4o` has pointed at different snapshots over time, and a
March request must resolve with March's mapping.

### 6.3 Requested versus served model

The reserve prices the requested model resolved through aliases — all that is
known before the call. The settle prices **the model the provider says it
served**, taken from the `model` field of the response.

Both are stored. When they differ beyond a known alias resolution,
`costlane_model_mismatch_total` increments, because it means billing rested on a
wrong assumption.

### 6.4 Unpriced models

| Situation | Behaviour |
|---|---|
| Neither alias nor canonical exists | **404** `model_not_found` |
| Model known, no active price, key **has** a budget | **400** `model_not_priced` |
| Model known, no active price, key **has no** budget | 200, `cost_usd = NULL`, `unpriced = true` |

Reserve-and-settle presupposes a price: a model without one cannot be reserved,
so refusing explicitly beats letting untracked spend through. The two error
codes are distinct because they call for different fixes — "you used the wrong
name" versus "a row is missing from pricing".

### 6.5 Source verification

A weekly CI job fetches each provider's public pricing page and asserts that
every active rate still appears. It opens an issue rather than failing the
build: a red build caused by a provider's markup change is noise that teaches
people to ignore CI. **If a page is unreachable, the issue says so** — an
unverifiable rate must not be reported as a verified one.

### 6.6 Usage records

```
usage_records
  id, key_id, reservation_id, request_id
  requested_model, served_model, provider, price_id
  provider_request_id text NULL       -- provider's x-request-id
  window_start date                   -- window of the RESERVE
  input_tokens, cached_read_tokens, cache_write_tokens, output_tokens
  cost_usd numeric(20,10) NULL
  usage_source text                   -- provider | tokenizer | estimate
  unpriced, streamed, client_disconnected, drain_timeout  bool
  parse_errors int
  finish_reason text NULL
  status_code int
  error_code text NULL
  latency_ms, ttft_ms int
  created_at timestamptz
```

`provider_request_id` is the only key for disputing an invoice or opening a
ticket with a provider, and is captured even when the request fails.
`error_code` is separate from `status_code` because a stream that fails after
headers are sent reports 200 to the client while still having failed.
`ttft_ms` is recorded because on a stream the total latency says nothing about
perceived responsiveness.

Writes go to an in-memory buffer flushed in batches, off the critical path. The
budget decrement stays synchronous and durable. Records buffered at the moment
of a hard crash are lost; graceful shutdown flushes them, so this applies only
to hard crashes, and reconciliation query 2 measures it.

Indexing: BRIN on `created_at`, since the table is append-only and BRIN costs a
fraction of a btree for range queries; btree on `(key_id, created_at)` for
per-key access. No foreign key points **at** `usage_records`, `created_at`
appears in every unique index, and a configurable retention job deletes beyond N
days — three properties that make later partitioning a mechanical change.

### 6.7 Prompt caching normalisation

Providers report cached tokens differently. OpenAI returns
`prompt_tokens_details.cached_tokens` as a **subset** of `prompt_tokens`, so it
must be subtracted to avoid double counting. Anthropic returns
`cache_read_input_tokens` and `cache_creation_input_tokens` as fields
**separate** from `input_tokens`. Both are normalised into the four columns,
with tests asserting that the parts reconcile with the provider's total.

---

## 7. API

### 7.1 Authentication

Credentials arrive in `Authorization: Bearer`, never in a query string.

A master key from `COSTLANE_MASTER_KEY` compares in constant time and can do
everything. A virtual key, formatted `cl_<32 bytes base62>`, proxies requests
and reads its own usage.

Virtual keys are stored as SHA-256 hashes with a unique index, plus an
eight-character `key_prefix` for display. The secret is shown once, at creation.
SHA-256 rather than Argon2 because verification is on the critical path of every
request and the key is already high-entropy; a deliberately slow KDF would tax a
proxy that sells low latency.

### 7.2 Routes

```
POST /v1/chat/completions      virtual key
GET  /v1/models                virtual key

POST   /admin/keys             master
GET    /admin/keys             master
PATCH  /admin/keys/{id}        master
DELETE /admin/keys/{id}        master   (soft revoke)
POST   /admin/pricing/reload   master
GET    /admin/reconcile        master

GET /v1/usage                  master sees all; virtual key sees itself
GET /v1/usage/series           idem
GET /v1/usage/requests         idem
GET /v1/budget                 idem
```

`GET /v1/models` returns models with an active price, in OpenAI format, read
locally with no upstream call. Clients that enumerate models at startup need it.

Scope isolation lives in the repository layer, not the handler: query functions
take a scope parameter that forces `WHERE key_id = ?` for a virtual key. A
handler that forgets to filter does not compile, because the parameter is not
optional.

`GET /v1/usage` accepts `group_by` from a whitelist enum (`key`, `model`,
`provider`, `day`, `hour`), at most two dimensions combined, with a mandatory
time range whose maximum width is configurable (default 90 days) and a dedicated
`statement_timeout` on the read pool (default 5s). No query can become a full
scan. Daily rollups are deferred.

Pagination is keyset on `(created_at, id)` with an opaque base64 cursor. On a
table growing by one row per request, `OFFSET` is a scan.

### 7.3 Virtual keys

```
virtual_keys
  id uuid PK
  key_hash bytea NOT NULL UNIQUE      -- SHA-256
  key_prefix text NOT NULL
  label text NOT NULL
  metadata jsonb NOT NULL DEFAULT '{}'   -- team, feature, customer
  allowed_models text[] NULL             -- NULL = all
  disconnect_policy text NOT NULL DEFAULT 'cancel'
  drain_timeout_ms int NULL
  revoked_at timestamptz NULL
  created_at timestamptz NOT NULL DEFAULT now()
```

Free-form `metadata` keeps attribution flexible: teams, features, and customers
are labels aggregated at query time rather than a rigid hierarchy.

`allowed_models` is what makes per-customer keys useful — "this key may use only
the cheap models" — and costs one column, already inside the fused reserve
statement.

Revocation is soft, so historical records stay readable and attributed.
**Revocation stops new requests, not settles in flight.** A request that started
a second before revocation completes its settle normally: the settle does not
check `revoked_at`, and must not. Everything spent is accounted for, always.

### 7.4 Errors

Always the OpenAI envelope, so client SDKs parse it unmodified:

```json
{"error": {"message": "...", "type": "budget_exceeded",
           "code": "budget_exceeded", "param": null}}
```

| Situation | HTTP | type |
|---|---|---|
| Budget exhausted | **402** | `budget_exceeded` |
| Model known but unpriced, budgeted key | 400 | `model_not_priced` |
| Model not allowed for this key | 403 | `model_not_allowed` |
| Unknown key or revoked | 401 | `invalid_api_key` |
| Unknown model or alias | 404 | `model_not_found` |
| Provider error | passthrough | provider's own |
| Provider timeout | 504 | `upstream_timeout` |

402 rather than 429 because retrying does not help: a budget does not free
itself.

When a stream has already sent 200 headers and then fails, the status cannot
change. An SSE error event is emitted in the same envelope, followed by `[DONE]`.
The usage record carries `status_code = 200` — what the client saw — with
`error_code` populated.

### 7.5 Secrets and logging

Provider keys come only from the environment, are never logged, never returned,
and never appear in an error message — including when a provider echoes them
back in its own error body. Virtual keys appear in logs only as `key_prefix`.

Prompts are logged only when `COSTLANE_LOG_PROMPTS=true`, into a separate
`request_payloads` table that can be dropped without touching spend queries.

A CI leak test plants recognisable synthetic keys in the environment and in fake
provider responses, exercises every error path, and fails if the pattern appears
in logs, responses, or metrics.

### 7.6 Metrics

`/metrics` is served without authentication on a separate address
(`COSTLANE_METRICS_ADDR`), so it is not exposed alongside the API. Labels stay
low-cardinality — `model`, `provider`, `status` — and never include `key_id`,
which would overwhelm Prometheus.

```
costlane_requests_total{model,provider,status}
costlane_request_duration_seconds        histogram
costlane_ttft_seconds                    histogram, streams only
costlane_proxy_overhead_seconds          histogram
costlane_tokens_total{model,kind}
costlane_budget_rejections_total
costlane_budget_overshoot_total
costlane_budget_drift_usd                gauge
costlane_reservations_expired_total
costlane_usage_records_dropped_total
costlane_model_mismatch_total
costlane_unpriced_requests_total
```

---

## 8. Operations

### 8.1 Configuration

All configuration comes from environment variables, with no cloud-specific
dependency. Validation runs at boot and fails loudly:
`drain_timeout <= provider_timeout`, and
`TTL >= provider_timeout + drain_timeout + margin`.

### 8.2 Migrations

Numbered `.sql` files embedded with `embed.FS`, executed by goose used as a
library rather than an external binary: version table, ordering, per-migration
transactions, and down migrations are already solved and tested. An advisory
lock guards against concurrent application by N replicas.

`COSTLANE_MIGRATE_ON_BOOT` defaults to true for development. In production,
migrations run as a separate step before rollout, not from every starting
replica.

Two cases need care: `CREATE EXTENSION btree_gist` requires privileges the
application user may lack, and `CREATE INDEX CONCURRENTLY` cannot run inside a
transaction. goose supports both through annotations.

### 8.3 Graceful shutdown

Treated as a tested feature, not a detail. On SIGTERM: stop accepting new
requests, wait for in-flight settles up to N seconds, flush the usage buffer,
exit. With this, buffered-record loss applies only to hard crashes, not to every
deploy.

### 8.4 Container and probes

A distroless or scratch base image running as a non-root user.
`/healthz` reports process liveness; `/readyz` requires a reachable database and
a loaded price snapshot. Readiness fails when prices are not loaded, because we
do not want to serve requests we cannot price.

A `docker-compose.yml` runs the gateway, Postgres, and the fake provider
together, so `docker compose up` yields a working gateway with no real
credentials. The first command in the README must work without spending
anything.

---

## 9. Testing

Test-driven throughout: a failing test, then implementation, then refactor.

**Unit tests, no I/O.** Cost arithmetic across the four token classes, context
tiers, and caching normalisation for both OpenAI and Anthropic conventions.
Alias resolution including historical remapping. SSE parsing: frames split
across reads, `\r\n` versus `\n`, unterminated frames, the 1 MB cap.

**Fuzzing.** `go test -fuzz` on the SSE parser, seeded from the fake provider's
scenarios. It is a byte-level parser over external input, which is precisely the
case fuzzing exists for. CI runs a fixed 30-second budget; nightly runs longer.

**Integration with Postgres** via testcontainers, skipped automatically when
Docker is absent. Concurrent reserves from N goroutines on one key, asserting
that `spent + reserved <= limit` and that the number of successes is exactly as
predicted. Idempotent settle. Settle after expiry with no double release. A
reaper-versus-settle race where exactly one side wins. Window rotation with
reservations spanning the boundary. All three reconciliations returning zero
after a concurrent load. An overlapping price row rejected by the exclusion
constraint.

**Integration with the fake SSE provider**, in-process via `httptest`, covering
every row of the failure matrix in section 4.6, plus a client that closes its
socket mid-stream so that disconnection is exercised over real network
behaviour rather than a mock.

**Leak tests** as described in 7.5. **Race detector** across tests and
benchmarks alike.

**Benchmarks.** Overhead is the delta between calling the fake provider directly
and calling it through the gateway on the same host: `BenchmarkProxyOverhead`
(non-streaming), `BenchmarkStreamOverhead` (TTFT and per-chunk), and
`BenchmarkReserveContention` (N goroutines on one key). README figures come from
a dedicated machine with stated hardware, never from CI runners, which are too
noisy to cite.

### 9.1 CI

| Job | Blocking |
|---|---|
| `golangci-lint` (errcheck, bodyclose, gosec, staticcheck) | yes |
| `go test ./... -race` | yes |
| integration (testcontainers) | yes |
| fuzz, 30s budget | yes |
| leak test | yes |
| build and docker build | yes |
| benchmark, benchstat comparison posted to the PR | report only, except a single coarse gate at >100% p99 regression |
| pricing source verification, weekly | no, opens an issue |

The benchmark gate is deliberately coarse. A tight threshold on noisy shared
runners produces false positives, which teach people to ignore CI; a 100% gate
catches only catastrophes.

---

## 10. Accepted risks

**Buffered usage records are lost in a hard crash.** Graceful shutdown covers
deploys; reconciliation query 2 measures the residue. If it proves material, the
buffer becomes a durable outbox.

**The materialised balance can drift from the reservation log.** Guarded by three
reconciliation queries, a Prometheus gauge, and a concurrency test that asserts
equality after load.

**A single request may overshoot its budget.** Bounded to one request, flagged
per row, and counted. The alternative — injecting `max_tokens` — would alter
provider behaviour the client did not request.

**Estimates are approximate for non-OpenAI providers** when a stream is cut
short, since no exact local tokenizer exists. Every record declares its
`usage_source`, so an estimate is never mistaken for a measurement.

**Previous-month totals can grow after the reset**, bounded by TTL seconds, as
described in 5.8.
