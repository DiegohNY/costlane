# costlane

An OpenAI-compatible LLM gateway that meters tokens and cost per virtual key,
and stops a key that runs out of budget — in the request path, before the money
is spent.

[![CI](https://github.com/DiegohNY/costlane/actions/workflows/ci.yml/badge.svg)](https://github.com/DiegohNY/costlane/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

## Sixty seconds, no API keys

The compose file ships a fake provider that speaks the OpenAI, Anthropic and
Gemini dialects, so the whole thing runs offline and costs nothing.

```bash
git clone https://github.com/DiegohNY/costlane && cd costlane
docker compose up --build
```

Mint a virtual key with a five dollar limit:

```bash
curl -s localhost:8080/admin/keys \
  -H "Authorization: Bearer dev-master-key-not-for-production-0123456789" \
  -H "Content-Type: application/json" \
  -d '{"label":"demo","limit_usd":"5"}'
```

Use it. The response carries what the call cost and what is left:

```bash
curl -i localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer cl_...the key you just got..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}]}'
```

```
X-Costlane-Cost-Usd: 0.002
X-Costlane-Budget-Remaining-Usd: 4.998
X-Costlane-Request-Id: 0e4253ac-a941-4826-be5c-16fc3b9497cb
```

Ask what has been spent:

```bash
curl -s "localhost:8080/v1/usage?group_by=model,day" \
  -H "Authorization: Bearer cl_...the key..."
```

Then spend the rest of the budget in a loop and watch the gateway return
`402 Payment Required` with the limit, the spend and the window reset in the
body.

## Connecting a real provider

Point the gateway at the real endpoints and give it real credentials. Nothing
else changes: the same virtual keys, the same budgets, the same read API.

```bash
COSTLANE_OPENAI_API_KEY=sk-...
COSTLANE_OPENAI_BASE_URL=https://api.openai.com

COSTLANE_ANTHROPIC_API_KEY=sk-ant-...
COSTLANE_ANTHROPIC_BASE_URL=https://api.anthropic.com

COSTLANE_GOOGLE_API_KEY=...
COSTLANE_GOOGLE_BASE_URL=https://generativelanguage.googleapis.com
```

A provider with no key is simply not configured, so a deployment can run with
one. Generate a real master key with `openssl rand -hex 32`; anything shorter
than 32 characters is refused at boot.

On the client side, change `base_url` and use a virtual key as the API key.
The SDK stays as it is:

```python
client = OpenAI(base_url="https://costlane.internal/v1", api_key="cl_...")
```

## What it does that the alternatives do not

**It stops the spend, in the path.** The budget check is a predicate inside the
statement that takes the reservation, so a key that is out of budget gets a 402
instead of a completion — not a dashboard alert after the fact, and not an
email tomorrow. It holds under concurrency: a thousand parallel requests
against one key cannot overspend it, because the check and the write are the
same statement ([ADR 0003](docs/adr/0003-reserve-in-one-statement.md)).

**Streaming is accounted for exactly, not estimated.** The gateway asks the
provider for its usage figures, reads them as the stream goes past, and strips
the chunk again if the client never asked for it. Cost comes from the
provider's own numbers, not from a tokeniser guessing at them.

**Prices are versioned, sourced and dated.** One row per token kind, per
context tier, per validity window, each carrying the URL it was read from and
the date it was read. A daily job re-fetches every source and fails the build
when a page changes, so a price change is a red build rather than a surprise
invoice ([ADR 0002](docs/adr/0002-long-price-format.md)).

**Amounts are decimal strings, everywhere.** A JSON number would be parsed as a
double by most clients and rounded. This product exists to be exact.

## Behaviour worth knowing about

**Request bodies reach OpenAI-compatible providers byte for byte.** costlane
does not parse and re-serialise them, so a parameter it has never heard of
still arrives intact.

**Parameters with no equivalent are refused, not dropped.** Asking Anthropic
for `logprobs`, or Gemini for `seed`, returns 400 naming the parameter. A
client that receives a response cannot tell a silently discarded field from one
the model ignored, so costlane does not create that ambiguity.

**`max_tokens` is injected for Anthropic only.** The Messages API rejects a
request without it, so costlane supplies `COSTLANE_DEFAULT_MAX_TOKENS` (4096 by
default) and says so in `X-Costlane-Injected-Max-Tokens`. It is the same figure
the reservation uses, so the amount reserved and the ceiling actually applied
agree by construction.

**A client that disconnects mid-stream cancels the upstream call.** Providers
stop generating when the connection closes, and draining to learn an exact
figure means paying for tokens nobody will read. Set a key's
`disconnect_policy` to `drain` to buy exactness at that price
([ADR 0001](docs/adr/0001-cancel-not-drain.md)).

**An exhausted budget is 402, never 429.** Retrying does not help; the body
carries the limit, the spend, the outstanding reservations and when the window
resets.

**Days and hours are cut on UTC boundaries**, matching the budget windows. A
day that moved with the caller's timezone would give totals that never
reconcile with recorded spend.

**Crossing 272,000 input tokens reprices the whole OpenAI request** — every
token, not only those past the boundary. A reservation whose estimate lands
within a quarter of that cliff reserves at the higher rate, so a request near
the edge is refused rather than under-reserved.

**An unpriced model is refused on a budgeted key.** Cost that cannot be
computed cannot be capped. On a key with no limit the request proceeds and is
recorded with a null cost — unknown, never zero.

**Usage records are never dropped.** They leave the request path through a
bounded buffer; when it fills, the write goes synchronously instead. The
request pays for the congestion and nothing disappears. A test takes the
database away for three seconds under load and requires every record to
survive.

**Shutdown reports unready before it stops accepting.** On SIGTERM `/readyz`
turns 503 immediately so a load balancer stops routing here while requests can
still be served. `/healthz` stays 200 throughout — and reports the build
version — because a dependency being down is not a reason to be restarted.

## Known limitations

Stated plainly, because finding them yourself would be worse.

- **Live-provider behaviour is verified but narrowly.** The cancel policy and
  the token counts are checked against real APIs in
  [provider verification](docs/provider-verification.md); everything else is
  tested against a fake provider.
- **Vertex AI and Bedrock are not supported.** Google means the Gemini API with
  an API key. Vertex needs a different auth flow and a different URL shape, and
  neither is seeded or tested.
- **Google does not stream.** The adapter completes; it has no streaming path,
  so a streamed request to a Gemini model is refused rather than buffered.
- **Translation covers chat completions and tool calls.** Not embeddings, not
  images, not audio, not the Assistants or Responses APIs. A parameter with no
  equivalent is refused by name rather than dropped, so what is unsupported is
  visible rather than silent.
- **One master credential, no user accounts.** A virtual key identifies an
  application, not a person. There is no rate limiting and no per-user
  attribution.
- **Prices live in embedded YAML, not in Postgres.** A second copy in the
  database would be a second source of truth that could disagree with the one
  in use.
- **Budgets are monthly, on UTC.** No daily or per-hour caps yet.
- **No caching, no failover, no evaluation suites, no frontend.** Out of scope
  for v1 on purpose.

## Benchmarks

_Figures pending: they are produced by the `bench` job on the pull request
that introduces this section, and pasted here from its step summary before the
release is tagged._

### Methodology

Everything below is measured by the `bench` job in
[`.github/workflows/ci.yml`](.github/workflows/ci.yml), so the method is a file
rather than a paragraph, and the hardware is one anyone can rent.

- **Hardware:** GitHub-hosted `ubuntu-latest` runner (4 vCPU, 16 GB).
- **Go:** 1.27. **Postgres:** 17-alpine, started by testcontainers.
- **Provider:** the in-repo fake provider over loopback, answering with fixed
  token counts and no latency, so the only variable is the gateway. Streamed
  responses carry 200 chunks.
- **Warm-up:** `testing.B` discards its own ramp-up iterations; the database
  and the HTTP connection pools are established before the timer starts.
- **Runs:** `-count 6`, compared with
  [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat).
- **Percentiles** are computed per iteration inside the benchmark and reported
  as custom metrics, because a mean hides the tail and the tail is what an
  operator feels.

Reproduce any of it locally (Docker required for the last two):

```bash
go test ./internal/proxy/ -run '^$' -bench 'NonStream|Stream(Direct|Gateway)' -count 6
go test ./internal/store/ -run '^$' -bench 'Reserve|Settle' -count 6
```

### 1. Non-streaming overhead

The same call, direct to the provider and through the gateway. The delta is
authentication, pricing, the reserve, the settle and the accounting.

| | p50 | p99 |
|---|---|---|
| Direct to provider | _pending_ | _pending_ |
| Through costlane | _pending_ | _pending_ |
| **Overhead** | _pending_ | _pending_ |

### 2. Streaming overhead

Time to first token, and the per-chunk cost of parsing every frame on the way
past.

| | TTFT p50 | TTFT p99 | Per chunk p50 | Per chunk p99 |
|---|---|---|---|---|
| Direct to provider | _pending_ | _pending_ | _pending_ | _pending_ |
| Through costlane | _pending_ | _pending_ | _pending_ | _pending_ |
| **Overhead** | _pending_ | _pending_ | _pending_ | _pending_ |

### 3. Reserve under contention

Many requests racing on one key's budget row, against the same work spread
across sixty-four keys. The gap is Postgres serialising updates to a single
row, which is the cost the design accepts in exchange for a budget that cannot
be exceeded.

| | ns/op |
|---|---|
| One key, contended | _pending_ |
| Sixty-four keys, uncontended | _pending_ |
| Settle | _pending_ |

### The shape behind the numbers

**One database round trip stands between an accepted request and the provider
call.** Authentication, the model allowlist, the monthly window rotation and
the budget check are all predicates inside the single statement that takes the
reservation. The settle and the remaining-budget read happen after the provider
has already answered; the usage record leaves the request path entirely,
through a bounded buffer.

That is not an assertion — it is
[a test that counts the statements a request issues](internal/proxy/queries_test.go)
and fails if anything is added in front of the reserve.

## Documentation

- [Design document](docs/superpowers/specs/2026-09-09-costlane-design.md) —
  architecture, streaming model, budget concurrency, pricing, API.
- [Architecture decision records](docs/adr/) — the seven decisions that
  reversed an earlier one, and why.
- [Provider verification](docs/provider-verification.md) — what was checked
  against live APIs, with request ids.
- [Security](SECURITY.md) — threat model, and how to report a vulnerability.
- [Implementation plan](docs/superpowers/plans/IMPLEMENTATION.md) — phased,
  with progress tracked in-repo.

## License

Apache-2.0. See [LICENSE](LICENSE).
