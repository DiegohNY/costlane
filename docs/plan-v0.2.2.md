# costlane v0.2.2 — implementation plan

**Released:** v0.2.1 on 2026-09-11.
**Rule:** TDD throughout — failing test first, implementation second.
**Rule:** nothing is "done" until every box in the item is ticked.

Legend: `[ ]` todo · `[x]` done · `[~]` in progress · `[!]` blocked

Two items. The order is not arbitrary: the first is money a user is spending
and cannot stop, the second is a database round trip nobody can see.

**Total estimate: 2–3 days**, of which half a day is waiting on a free-tier
quota.

---

## W1 — Map `reasoning_effort` to `thinkingConfig` (#24)

**Estimate: 1½–2 days.**

Goal: a caller can tell Gemini not to think, and stop paying for thinking they
did not ask for.

`TranslateGoogleRequest` builds `generationConfig` from a fixed set of OpenAI
fields — `max_tokens`, `temperature`, `top_p`, `stop`. Gemini's
`thinkingConfig` has no path through it, and a `thinkingConfig` in an incoming
body is not refused either: it is simply ignored, because it is not on the
unsupported denylist. So a caller gets whatever thinking budget the model
defaults to, and is billed for it.

Google bills thinking as output. Two live measurements:

| Request | Visible output | Thinking | Billed as output |
|---|---|---|---|
| "Reply with exactly: ok" | 1 | 172 | **173** |
| a short reasoning prompt, streamed | 50 | 279 | **329** |

A one-word answer costing a hundred and seventy-three tokens is not a rounding
error, and a cost-control gateway that cannot pass through the parameter
controlling it is not offering the control it advertises.

It also contradicts a promise costlane makes elsewhere, in the README and in
the translation tests: **a parameter with no equivalent in the destination is
refused by name rather than dropped.** `thinkingConfig` is dropped. So is
`reasoning_effort`.

### The mapping

- [ ] Read Google's current documentation for `thinkingConfig.thinkingBudget`:
      the valid range, what `0` means, what `-1` (dynamic) means, and whether
      the range differs per model
- [ ] Decide the level-to-budget table and record each figure with a
      `source_url` and a `fetched_at`, in the shape the price seed uses. **A
      number with no source does not enter this repository** — that rule is
      what makes the price table auditable, and it applies here too
- [ ] Seed it as data rather than as constants in a switch, so a change is a
      data change with provenance rather than a code change without

Shape to fill in from the documentation, not from memory:

| `reasoning_effort` | `thinkingBudget` | source |
|---|---|---|
| `none` | 0 | _to be read_ |
| `minimal` | ? | _to be read_ |
| `low` | ? | _to be read_ |
| `medium` | ? | _to be read_ |
| `high` | ? | _to be read_ |
| absent | absent — the model's own default | n/a |

- [ ] If a model does not accept a budget at all, that is a refusal by name,
      not a silent drop

### The implementation

- [ ] `TranslateGoogleRequest` reads `reasoning_effort` and emits
      `generationConfig.thinkingConfig.thinkingBudget`
- [ ] The streaming path uses the same translation — it already shares
      `TranslateGoogleRequest`, so this should come for free, and there should
      be a test that says so rather than an assumption that it does
- [ ] `reasoning_effort` against OpenAI passes through untouched: it is that
      dialect's own field, and the body reaches OpenAI byte for byte
- [ ] `reasoning_effort` against Anthropic is **refused by name**, unless the
      documentation gives an equivalent — in which case it is mapped, and the
      mapping is sourced like Google's
- [ ] A `thinkingConfig` sent directly in an OpenAI-dialect body is refused by
      name rather than ignored, which is the behaviour every other unsupported
      parameter already gets

### The tests

- [ ] Golden fixtures for the translated request at each level, one file per
      case, so what is supported is enumerable rather than folded into prose
- [ ] The fake provider honours `thinkingConfig.thinkingBudget`: with `0` it
      reports no `thoughtsTokenCount`, so the mapping is covered end to end
      without spending anything
- [ ] TEST: a request with `reasoning_effort: none` through the whole gateway
      records `reasoning = 0` and an output count equal to the visible tokens
- [ ] TEST: each refusal above, by name, with the parameter in the message
- [ ] VERIFY, against the live API: `reasoning_effort: none` on
      `gemini-3.8-flash` returns `usageMetadata` with no `thoughtsTokenCount`.
      One request. Recorded in `docs/provider-verification.md` beside the
      others

### What this unblocks

- [ ] **Check (c) of the provider verification becomes decisive.** Today the
      cancellation check asks for `max_tokens: 4000` and cancels after the
      first chunk — but on a thinking model the first chunk arrives only once
      the thinking is done, so 2566 of the 4000 tokens were already produced
      before a cancel was possible. The intended contrast of 1 against 4000 is
      really 2566 against 4000.

      With `thinkingBudget: 0` the first chunk is a real output token and the
      contrast returns: **1 against 4000**, which is the comparison the check
      was designed around.
- [ ] Re-run check (c) with thinking disabled, and record the result **beside**
      the indicative one rather than replacing it. The old row stays: a
      measurement that was honest about its limits is not deleted because a
      better one arrived

**Why 1½–2 days.** The translation is an afternoon. The mapping table is the
part that takes care — every figure needs a source, and the levels may not map
cleanly onto a numeric budget. The live verification costs one request and a
day's patience if the free tier is exhausted, which on twenty requests per day
per model it often is.

---

## W2 — Budget remaining from the settle's `RETURNING` (#14)

**Estimate: 1 day.** Unchanged from the v0.2 plan, where it was V3.

Goal: one fewer synchronous statement on the happy path.

The trace in `internal/proxy/queries_test.go` shows a `SELECT` after the settle
whose only job is to fill `X-Costlane-Budget-Remaining-Usd`, reading the row
the settle updated one statement earlier.

- [ ] `RETURNING limit_usd, spent_usd, reserved_usd` on the settle's second
      `UPDATE`; the remaining figure lands in `SettleOutput`
- [ ] The non-streaming path fills the header from it
- [ ] The streaming path settles too, and its header handling moves with it —
      though note that a streamed response carries no cost header at all,
      because the cost is not known until the stream ends and the headers left
      before it began
- [ ] Decide and test the `Applied: false` case: a retried settle, or one the
      reaper beat, has no row to return from. Omitting the header is
      defensible — the request is in a state where the figure is not the
      settle's to report — but it must be a decision with a test
- [ ] `RemainingBudget` stays for the read API, which still needs it
- [ ] TEST: `queries_test.go` asserts four statements on the happy path rather
      than five. That test exists to make an addition to the path deliberate;
      a removal is recorded the same way
- [ ] Re-run the overhead benchmarks and update the README figures if the
      delta is real rather than noise

**Why 1 day.** The change is small and the edge case is the whole cost. A
settle that returns nothing has to leave the header in a defined state on both
paths, which is two code paths and four tests.

---

## Not in v0.2.2

- **OpenAI and Anthropic verification**
  ([#13](https://github.com/DiegohNY/costlane/issues/13)). Still blocked on
  funded credentials, not on work.
- **The Interactions API**
  ([#21](https://github.com/DiegohNY/costlane/issues/21)). Not urgent while
  the captured endpoint keeps working; it stops being non-urgent the moment
  Google announces a date.
- **Vertex AI and Bedrock.** Different auth flows, different URL shapes,
  unverifiable rates. Wanted, not scheduled.

## Cross-cutting invariants

Checked at every review, unchanged:

- [ ] No provider key in any log, response, or error message
- [ ] Every money column named `_usd` and typed NUMERIC
- [ ] Every window computation uses `AT TIME ZONE 'UTC'`
- [ ] Reserve, settle, and reap are each exactly one transaction
- [ ] Test written before implementation
- [ ] No test commit on `main`, ever

One more, earned in v0.2.1:

- [ ] **Every seam that only production crosses has a test that crosses it.**
      `buildRouter` went two releases without one, and a provider that nothing
      constructed answered 404 for every model it owned.
