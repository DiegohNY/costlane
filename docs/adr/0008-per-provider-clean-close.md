# 0008 — Each provider decides what a clean close looks like

Status: accepted. Supersedes treating a missing `[DONE]` as truncation.

## Context

A streamed response has to be told apart from a streamed response that was cut
off, because the second one is an incident and the first is a Tuesday. The
usage record carries `error_code = stream_truncated` for the second.

The first implementation had one test for it: the OpenAI sentinel. If the pump
never saw `data: [DONE]`, the stream was truncated.

That was written when OpenAI was the only dialect, and it survived Anthropic
because the pump appends a `[DONE]` of its own to any translated stream — an
OpenAI client waits for one, and Anthropic sends none. The check was therefore
passing on a sentinel the gateway had written itself.

Gemini made it visible. Its streams end by closing the connection after a
chunk carrying a `finishReason`; there is no sentinel at all. Under the old
test every healthy Gemini stream would have been recorded as truncated — an
error code on a completely normal request, which is worse than no error code,
because it teaches whoever reads the table to ignore the column.

## Decision

The adapter answers for its own protocol. `provider.Stream` gained:

```go
ClosedCleanly func() bool
```

- **OpenAI** leaves it nil, which means "the pump's `[DONE]` check is right" —
  and for a same-dialect stream, it is.
- **Anthropic** reports whether `message_stop` went past.
- **Gemini** reports whether a chunk carrying a `finishReason` went past. EOF
  is not an interruption for this provider; it is the ordinary end.

An EOF with no `finishReason` is still a truncation, and the Gemini tests cover
both halves.

## Alternatives rejected

**Keep the `[DONE]` check and let the pump's own appended sentinel satisfy it.**
What was effectively happening. It makes the check unfalsifiable: it passes
because we wrote the thing it looks for.

**Treat every EOF as clean.** Then a stream cut off mid-generation records no
error, which is the failure this field exists to catch.

**Infer cleanliness from whether usage arrived.** Tempting, since a provider
usually reports usage at the end — but Gemini reports it on every chunk, so an
abandoned stream would look complete. The two questions are genuinely
different: one is "did the model finish", the other is "do we know what it
cost".

## Consequences

- The pump keeps recording `SawDone`, which remains the answer for
  same-dialect streams and is what a nil `ClosedCleanly` falls back to.
- A new adapter that forgets to set it gets the OpenAI behaviour, which for a
  provider with no `[DONE]` means every stream is flagged. Loud in the right
  direction: it will be noticed on the first test.
- The related question — whether the counts are exact after an early exit — is
  a separate field, `UsageIsCumulative`, described in
  [ADR 0001](0001-cancel-not-drain.md). Two protocol facts, two fields, rather
  than one flag meaning several things.
