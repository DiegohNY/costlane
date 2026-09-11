# 0009 — A reasoning level is mapped from a sourced table, or refused

Status: accepted. Supersedes mapping `reasoning_effort` onto a numeric
thinking budget, and supersedes dropping it.

## Context

A client using the OpenAI dialect asks for less reasoning with
`reasoning_effort`. Until now costlane did nothing with it. The field was not
on any provider's unsupported list, so it was parsed by nobody and forwarded
to nobody: Gemini applied its own default thinking budget, and Google bills
thinking as output. A one-word answer measured 173 output tokens, of which 172
were thought.

That is a silent failure of the promise this gateway makes everywhere else —
**a parameter with no equivalent in the destination is refused by name rather
than dropped** — and it failed in the direction that costs money.

The plan for closing it assumed the mechanism was a number. Gemini 2.5 takes
`thinkingConfig.thinkingBudget`, an integer, with `0` meaning no thinking; the
work item was written as a table of budgets per model.

Reading the current documentation before writing the table changed the shape
of the work. Gemini 3.x replaced the budget with a string enum,
`thinkingConfig.thinkingLevel`, and the Gemini 3.8 Flash page instructs
readers to "Replace `thinking_budget` with the string enum `thinking_level`".
Two further facts followed from the same pages, and neither was in the plan:

- **`minimal` is not universal.** It "is not supported for Gemini 3.8 Flash
  and will return an error", while Gemini's own `reasoning_effort` mapping
  table publishes `minimal → low` for Gemini 3.1 Pro and no column for 3.8
  Flash at all.
- **Thinking cannot be switched off.** "Reasoning cannot be turned off for
  Gemini 2.5 Pro or 3 models." The `none` level, which the whole work item was
  designed around, has no destination on either seeded model.

Anthropic went the other way. It publishes `output_config.effort` — an enum of
`low`, `medium`, `high`, `xhigh` and `max`, defaulting to `high`, on every
model this repository prices. The parameter that was to be refused there has a
real equivalent after all.

## Decision

Levels are **data with provenance**, and an unmappable level is a **refusal**.

The mapping lives in `internal/provider/seed`, one file per provider, one row
per level, each row carrying the URL it was read from and the date it was read
— the same shape and the same rule as the price seed
([ADR 0002](0002-long-price-format.md)). The destination's own spelling is a
per-row value rather than a field name, because the same OpenAI word becomes
`thinkingLevel` for Gemini and `output_config.effort` for Anthropic.

Three things then fail closed, all of them ending in `400
unsupported_parameter` naming the level asked for:

1. a level with no row for that model — `none` on either Gemini 3 model,
   `minimal` on Gemini 3.8 Flash;
2. a model with no table at all;
3. a seed that did not parse.

A request that says nothing about reasoning grows nothing: the model applies
its own documented default, which is not costlane's to choose.

## Alternatives rejected

**Map `none` to the lowest level available.** The caller asked for no thinking
and would be given thinking, billed for it, and told nothing. That is the exact
failure this ADR exists to end, re-entering through the door marked
convenience.

**Map `minimal` to `low` on Gemini 3.8 Flash by analogy with 3.1 Pro.** The
analogy is ours, not Google's. A rate or a level that this repository derived
rather than read is indistinguishable from one it invented, which is why
[ADR 0002](0002-long-price-format.md) requires a source per row.

**Keep the numeric budget the plan assumed.** No numeric budget for these
models could be sourced, and the model page says to replace it. Writing one
anyway would put a number with no provenance into the repository.

**Refuse `reasoning_effort` for Anthropic too, for symmetry.** Symmetry is not
a reason to refuse a parameter the destination documents. The refusal rule
applies to parameters with no equivalent; Anthropic has one.

**Refuse only at the provider, and let the upstream 400 speak.** An upstream
refusal costs a round trip, arrives in the provider's vocabulary rather than
the caller's, and — for `none` — would not arrive at all, because Gemini
accepts the request and thinks anyway.

## Consequences

- A caller can lower Gemini's thinking but cannot stop it, and learns which of
  the two they got, in the response, by name. The README's limitation section
  says so rather than leaving it to be discovered.
- Check (c) of the [provider verification](../provider-verification.md) gains a
  sharper contrast but not the one it was designed around: `low` replaces the
  model's default, and `none` remains unavailable on any model costlane
  serves.
- Adding a model means adding rows with sources, not editing a switch. A
  model absent from the table refuses every level rather than silently
  accepting all of them.
- The seed is parsed once, and a parse failure maps nothing — so a malformed
  file refuses these requests rather than forwarding them unconfigured.
