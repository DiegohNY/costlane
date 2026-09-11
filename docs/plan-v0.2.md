# costlane v0.2 — implementation plan

**Released:** v0.1.0 on 2026-09-10.
**Rule:** TDD throughout — failing test first, implementation second.
**Rule:** nothing is "done" until every box in the item is ticked.

Legend: `[ ]` todo · `[x]` done · `[~]` in progress · `[!]` blocked

Three items, in this order. The order is not arbitrary: the first unblocks
the second, and the third is independent and small enough to fill a gap.

**Total estimate: 6–8 days**, of which roughly a day is waiting for provider
dashboards rather than working.

---

## V1 — Gemini streaming adapter (#12)

**Estimate: 3–4 days.**

Goal: a streamed request to a Gemini model is relayed chunk by chunk and
metered exactly, like the other two providers.

This is the largest item and the one that unblocks V2. It is also smaller
than it looks, because the fake provider already speaks streaming Gemini:
`internal/fakeprovider/stream.go` emits `streamGenerateContent` frames,
honours the malformed-chunk and fail-after-N scenario headers, and reports
`thoughtsTokenCount` in the final `usageMetadata`. The upstream half of the
test rig exists; only the adapter does not.

- [x] `(*Google).Stream` hitting `:streamGenerateContent?alt=sse`
- [x] `*Google` satisfies `provider.Streamer`; the router stops refusing it
- [x] Translator: Gemini SSE frames to OpenAI chat completion chunks, in the
      shape of `internal/provider/stream_anthropic.go`
- [x] Golden fixtures pinning the mapping event by event, one file per case,
      so what is supported is enumerable rather than folded into prose
- [x] Usage from the LAST chunk seen, cumulative rather than summed —
      captured behaviour, not the documented one. Usage from the final chunk's `usageMetadata`, output including
      `thoughtsTokenCount` — the live verification found 121 thinking tokens
      against 1 visible one, so a translator that reads
      `candidatesTokenCount` alone would meter a request at under one
      percent of its cost
- [x] `Usage()` reports `reported: false` when the final chunk carries no
      `usageMetadata`, so accounting falls back to the in-flight count and
      records `usage_source = estimate` rather than inventing a figure
- [x] TEST: a streamed Gemini request is relayed and priced to the token
      against the fake provider
- [x] TEST: a malformed chunk mid-stream degrades rather than corrupting the
      count (`X-Fake-Malformed-Chunk-At`)
- [x] TEST: a stream that dies before its usage chunk
      (`X-Fake-Fail-After-Chunks`)
- [x] TEST: tool calls — they arrive whole in one chunk, so one delta
      carries name and complete arguments, and no accumulator is needed.
      Original note: tool calls, if Gemini streams `functionCall` parts — the
      Anthropic path reassembles partial JSON and requires the result to
      parse; this needs the same or an explicit refusal
- [x] `FuzzGoogleStreamTranslator`, wired into the PR job and the nightly
      run: it parses a third party's bytes, which is what fuzzing is for
- [x] README: remove "Gemini is non-streaming only" from known limitations
- [x] VERIFY: an end-to-end proxy test streams a Gemini model through the
      whole gateway and checks the accounting

**Unplanned, and found along the way.** Three defects that predate this work:
reasoning tokens marked every thinking request `partially_priced` although its
cost was complete; negative token counts from a provider passed straight into
the meter, where they become a free request; and the fake provider's Gemini
streaming dialect matched neither the captures nor the documentation, because
nothing had ever exercised it. All three are fixed here.

**Why 3–4 days.** The Anthropic translator took most of F6 and this is the
same shape of work with the fixture format and the fuzz harness already
decided. Gemini's frames are simpler than Anthropic's — no content-block
indices to reconcile — but its usage lives in a different place and its tool
call shape is unlike either of the other two. The estimate assumes tool-call
streaming turns out to need real work; if it can be refused by name for now,
this is 2 days.

---

## V2 — Verify cancel stops billing on real providers (#13)

**Estimate: 1–2 days**, most of it waiting.

Goal: the last assumption in the design stops being an assumption.

v0.1.0 ships with the cancel default unmeasured on every provider. The
harness exists (`internal/providerverify`) and the method is written up in
`docs/provider-verification.md`; what was missing was credentials, and for
Google a stream to cancel — which V1 supplies.

**Blocked on:** funded credentials for OpenAI and Anthropic. Roughly five
dollars each is ample; the checks cost cents. V1 must land first for the
Google row to be runnable at all.

- [~] Run check (a): done for Google; OpenAI and Anthropic still need a credential
- [~] Run check (b): done for Google; the other two still need a credential
- [~] Run check (c): done for Google; the other two still need a credential
- [x] Read what the provider recorded in the UTC window the test printed —
      for Google this came from Cloud Monitoring rather than a dashboard,
      which makes it a query rather than a screenshot
- [x] Fill the three result tables in `docs/provider-verification.md`, with
      the raw usage payload behind each row and every query used
- [ ] If a provider keeps generating after the connection closes: say so by
      name in the README's known limitations, and make `drain` the
      recommended policy for keys routed to it. A default that overspends is
      the one failure this product cannot excuse
- [x] README: replaced "not measured on any provider" with what was measured
- [x] VERIFY: every figure in the document comes from the provider rather
      than from its documentation. Google's cancellation row is recorded as
      *indicative* rather than as a verdict: zero output tokens were recorded
      for the cancelled request, which is consistent with generation stopping
      and equally consistent with output usage being written only on
      completion. Separating those needs a bill, and a free-tier project
      produces none

**Why 1–2 days.** The work was an hour. The waiting was not, though not for
the reason predicted: the lag that mattered was the free tier's twenty
requests per day per model, spent capturing fixtures, which cost a day
outright. Cloud Monitoring then answered in minutes what was expected to need
a human reading a dashboard — and a 503 spends a request too, which is why
the retry loops have caps.

**Unplanned, and found along the way.** Go caches a passing test, so a retry
loop reported a success an hour after it happened and handed over the UTC
window of the older run; `-count=1` is now in the documented command.
`COSTLANE_VERIFY_ONLY` runs one check rather than three, because on twenty
requests a day the difference matters. And the check's premise — one token
against four thousand — does not survive a thinking model, which produced
2566 of its 4000 before a cancel was even possible: see
[#24](https://github.com/DiegohNY/costlane/issues/24).

---

## V3 — Budget remaining from the settle's RETURNING (#14)

**Estimate: 1 day.**

Goal: one fewer synchronous statement on the happy path.

The trace in `internal/proxy/queries_test.go` shows a `SELECT` after the
settle whose only job is to fill `X-Costlane-Budget-Remaining-Usd`, reading
the row the settle updated one statement earlier.

- [ ] `RETURNING limit_usd, spent_usd, reserved_usd` on the settle's second
      `UPDATE`; the remaining figure lands in `SettleOutput`
- [ ] The non-streaming path fills the header from it
- [ ] The streaming path does the same — it settles too, and its header
      handling has to move with it
- [ ] Decide and test the `Applied: false` case: a retried settle, or one
      the reaper beat, has no row to return from. Omitting the header is
      defensible — the request is in a state where the figure is not the
      settle's to report — but it must be a decision with a test, not an
      accident
- [ ] `RemainingBudget` stays for the read API, which still needs it
- [ ] TEST: `queries_test.go` asserts four statements on the happy path
      rather than five. The test exists to make an addition to that path
      deliberate; a removal is recorded the same way
- [ ] Re-run the overhead benchmarks and update the README figures if the
      delta is real rather than noise

**Why 1 day.** The change is small and the edge case is the whole cost. A
settle that returns nothing has to leave the header in a defined state on
both the streaming and the non-streaming path, and that is two code paths
and four tests, not one.

---

## Not in v0.2

Stated so that nobody has to ask twice:

- **Vertex AI and Bedrock.** Different auth flows and different URL shapes.
  Wanted, not scheduled.
- **Prices mirrored into Postgres.** Still a second source of truth that
  could disagree with the one in use. It goes in when something actually
  reads prices from the database.
- **Daily and hourly budget windows.** Budgets stay monthly on UTC.
- **Caching, failover, evaluation suites, a frontend.** Out of scope for v1,
  and still out of scope.

## Cross-cutting invariants

Unchanged from v0.1.0, and checked at every review:

- [ ] No provider key in any log, response, or error message
- [ ] Every money column named `_usd` and typed NUMERIC
- [ ] Every window computation uses `AT TIME ZONE 'UTC'`
- [ ] Reserve, settle, and reap are each exactly one transaction
- [ ] Test written before implementation
- [ ] No test commit on `main`, ever
