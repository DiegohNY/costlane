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

## W1 — Map `reasoning_effort` to each provider's thinking control (#24)

**Estimate was 1½–2 days.** Implementation and documentation done in one
session; the live verification below is still outstanding, because there is no
credential on this machine.

Goal: a caller can tell Gemini to think less, and stop paying for thinking
they did not ask for.

`TranslateGoogleRequest` built `generationConfig` from a fixed set of OpenAI
fields — `max_tokens`, `temperature`, `top_p`, `stop`. Gemini's thinking
configuration had no path through it, and a `thinkingConfig` in an incoming
body was not refused either: it was simply ignored, because it was not on the
unsupported denylist. So a caller got whatever thinking budget the model
defaults to, and was billed for it.

Google bills thinking as output. Two live measurements:

| Request | Visible output | Thinking | Billed as output |
|---|---|---|---|
| "Reply with exactly: ok" | 1 | 172 | **173** |
| a short reasoning prompt, streamed | 50 | 279 | **329** |

### What the documentation actually said

**The title of this item was wrong, and reading the pages before writing the
table is what caught it.** Gemini 2.5 took a numeric
`thinkingConfig.thinkingBudget`; Gemini 3.x takes a string enum,
`thinkingConfig.thinkingLevel`, and the Gemini 3.8 Flash page instructs
readers to "Replace `thinking_budget` with the string enum `thinking_level`".
No numeric budget for the seeded models could be sourced, so none was written.

Two further facts arrived with it, and both removed a box this plan had
already ticked in its head:

- **Thinking cannot be disabled.** "Reasoning cannot be turned off for Gemini
  2.5 Pro or 3 models." The `none` level this item was designed around has no
  destination on either seeded model.
- **`minimal` is not universal.** "`minimal` thinking level is not supported
  for Gemini 3.8 Flash and will return an error", while Google's own
  `reasoning_effort` table publishes `minimal → low` for Gemini 3.1 Pro and no
  column for 3.8 Flash at all.

Anthropic went the other way: it publishes `output_config.effort`, an enum of
`low`, `medium`, `high`, `xhigh` and `max` on every model this repository
prices. The parameter this plan expected to refuse there has a real
equivalent, so it is mapped.

### The mapping

- [x] Read Google's current documentation for the thinking configuration: the
      shape, the accepted levels, and whether they differ per model. They do.
- [x] Record each level with a `source_url` and a `fetched_at`, in the shape
      the price seed uses
- [x] Seed it as data rather than as constants in a switch:
      `internal/provider/seed/google-thinking.yaml` and
      `internal/provider/seed/anthropic-effort.yaml`

Read from the documentation on 2026-09-11, not from memory:

| `reasoning_effort` | `gemini-3.8-flash` | `gemini-3.1-pro-preview` | source |
|---|---|---|---|
| `none` | **refused** | **refused** | [openai compatibility](https://ai.google.dev/gemini-api/docs/openai) — reasoning cannot be turned off for Gemini 3 models |
| `minimal` | **refused** | `low` | [gemini-3](https://ai.google.dev/gemini-api/docs/generate-content/gemini-3), [latest-model](https://ai.google.dev/gemini-api/docs/generate-content/latest-model) — an error on 3.8 Flash; published as `low` for 3.1 Pro |
| `low` | `low` | `low` | the model pages above |
| `medium` | `medium` | `medium` | the model pages above |
| `high` | `high` | `high` | the model pages above |
| absent | absent — the model's own default | absent | n/a |

And for Anthropic, `output_config.effort`, from
[effort](https://platform.claude.com/docs/en/build-with-claude/effort) on the
same date: `low`, `medium` and `high` map by name on `claude-fable-5-1`,
`claude-opus-5` and `claude-sonnet-5`; `none` and `minimal` are refused;
`xhigh` and `max` have no `reasoning_effort` spelling to arrive as.

- [x] If a model does not accept a level at all, that is a refusal by name,
      not a silent drop

### The implementation

- [x] `TranslateGoogleRequest` reads `reasoning_effort` and emits
      `generationConfig.thinkingConfig.thinkingLevel`. It now takes the routed
      model as an argument: a `provider/model` prefix is resolved before the
      adapter sees it, so the body's own `model` is the wrong key for the
      table.
- [x] The streaming path uses the same translation, with a test that says so
      rather than an assumption that it does — and a second test that a
      refused level never opens a connection
- [x] `reasoning_effort` against OpenAI passes through untouched
- [x] `reasoning_effort` against Anthropic is **mapped**, because the
      documentation gives an equivalent, and the mapping is sourced like
      Google's
- [x] `thinkingConfig` sent directly in an OpenAI-dialect body is refused by
      name, along with `thinkingLevel`, `thinking_level`, `thinkingBudget` and
      `thinking_budget`

### The tests

- [x] Golden fixtures for the translated request, one file per case, in
      `internal/provider/testdata/google`
- [x] TEST: each refusal above, by name, with the level in the message
- [x] TEST: a request with no `reasoning_effort` grows no thinking
      configuration at all
- [x] TEST: end to end through the gateway — a mapped level is accepted, an
      unmappable one is a 400 that names the parameter and the level. These
      four subtests need Docker and were **not run on this machine**; CI runs
      them.
- [~] The fake provider honours the thinking level. **Not done, and the
      reason is the mechanism change:** the plan wanted `thinkingBudget: 0` to
      produce no `thoughtsTokenCount`, and there is no budget `0` to send. A
      fake that invented a thought count per level would be asserting our own
      guess about Google's behaviour. What the level does on the wire is
      pinned by the golden fixtures and by a test that captures the upstream
      request body on the streaming path.
- [ ] VERIFY, against the live API: `reasoning_effort: low` on
      `gemini-3.8-flash` returns a `usageMetadata` with a `thoughtsTokenCount`
      below the default run's. One request. **Blocked: no credential.**

### What this unblocks

- [~] **Check (c) of the provider verification gets a sharper contrast, but
      not the one it was designed around.** The check asks for `max_tokens:
      4000` and cancels after the first chunk; on a thinking model the first
      chunk arrives only once the thinking is done, so 2566 of the 4000 tokens
      were already produced before a cancel was possible.

      With thinking disabled the contrast would return to 1 against 4000.
      **Thinking cannot be disabled on these models**, so the check moves to
      `reasoning_effort: low` and the contrast becomes whatever `low` costs —
      a number to be recorded from the run, not predicted here.
- [ ] Re-run check (c) at `reasoning_effort: low`, and record the result
      **beside** the indicative one rather than replacing it. The old row
      stays. **Blocked on the same credential.**

---

## W2 — Budget remaining from the settle's `RETURNING` (#14)

**Estimate was 1 day.** Done, except the benchmark re-run, which needs Docker.

Goal: one fewer synchronous statement on the happy path.

The trace in `internal/proxy/queries_test.go` showed a `SELECT` after the
settle whose only job was to fill `X-Costlane-Budget-Remaining-Usd`, reading
the row the settle had updated one statement earlier.

- [x] `RETURNING` on the settle's second `UPDATE`; the remaining figure lands
      in `SettleOutput`. It returns the computed remainder rather than the
      three columns, because that is the figure the header wants and the
      subtraction belongs where the NULL limit is handled
- [x] The non-streaming path fills the header from it
- [x] The streaming path settles too, and its header handling moves with it —
      which turned out to be nothing to move: a streamed response carries
      neither a cost nor a remaining header, because both are known only once
      the stream has ended and the headers left before it began. That is now
      a test rather than an accident
- [x] The `Applied: false` case is decided and tested. A settle that applied
      nothing returns no figure, and the header is omitted. It did not write
      the budget row, so it cannot speak for it, and a number read from
      another transaction's work would be presented as this request's own
- [x] ~~`RemainingBudget` stays for the read API~~ **It does not.** The read
      API answers through `BudgetStatusFor`, which has its own query; the
      proxy's response header was `RemainingBudget`'s only caller. It is
      deleted rather than left behind as a second way to ask the same
      question
- [x] TEST: `queries_test.go` asserts no `SELECT` on the happy path, and five
      statements in this harness — the four that do the work plus the usage
      `INSERT` that production hands to a buffer. That test exists to make an
      addition to the path deliberate; a removal is recorded the same way
- [ ] Re-run the overhead benchmarks and update the README figures if the
      delta is real rather than noise. **Not run: the benchmarks need Docker,
      which this machine does not have.** The README now says that its
      figures were measured with the extra read still in place, so nothing
      there overstates what was removed.

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
