# Show HN draft

**Not published. For review.** Current as of v0.2.1. Six paragraphs: what it
is, why in-path and how, how the metering is checked, the numbers, what it
does not do, and how the release pipeline is built.

---

**Title:**

> Show HN: Costlane – an LLM gateway that stops a key at its budget, in the request path

*(78 characters. HN truncates around 80.)*

---

**Body:**

Costlane is an OpenAI-compatible proxy that sits between an application and OpenAI, Anthropic or Google, meters every request exactly, and refuses one that would take a virtual key past its spending limit. The refusal happens before the provider is called, so the budget is a cap rather than a report: a key that is out of money gets a 402, not an alert tomorrow morning.

Per-key budgets exist elsewhere — LiteLLM has them. What I wanted was the correctness story: the cap enforced as a predicate inside the single UPDATE that takes the reservation, so a thousand parallel requests on one key cannot overspend it; streaming metered from the provider's own usage figures as the chunks pass, rather than from a tokeniser guessing at them; and prices as versioned data with an effective range, so a repricing never rewrites what past requests cost.

The metering is checked against live APIs rather than against itself. For Gemini, our count, the provider's own response payload and Google's Cloud Monitoring telemetry agree to the token on the same requests — three readings, one of them computed by a system with no idea we exist. That check matters more than it sounds: Google bills thinking tokens as output and reports them in a separate field, so a meter reading the obvious one would have billed a one-word answer at 1 token instead of 173 and passed every test written against the response body.

Measured overhead against a local fake provider is 1.96 ms at p50 on a GitHub runner, almost all of it Postgres rather than anything the gateway computes. Per streamed chunk it is 13 microseconds — read a frame, count its tokens, write it on.

What it does not do, since that matters more than what it does: the cancel-on-disconnect default has indicative evidence on Gemini — a cancelled request recorded zero output tokens in Google's telemetry — and is unmeasured on OpenAI and Anthropic, which needed credentials I did not have; the README says exactly that and offers `disconnect_policy: drain` to anyone who needs certainty rather than a default. costlane cannot currently ask Gemini to stop thinking, so a caller pays for a thinking budget they cannot set. There is no Vertex or Bedrock, one master credential and no user accounts, no rate limiting, no caching, no failover, no UI. `docker compose up` gives you the whole thing with a fake provider and no API keys, and the eight decisions that reversed an earlier one are written up as ADRs, including the ones I got wrong first.

One more thing about how it is built, because it is the most honest thing I can show you. The release pipeline pushes the version tag, boots that exact image against a real Postgres, and only then re-points `:latest` at the digest that passed. The first time it ran, it failed — for a reason unrelated to the image — and the text of its own failure exposed that Gemini had never been routable from the binary in two releases, despite a fully tested streaming adapter. `:latest` stayed on the previous release the whole time. The repository keeps a file of those, `docs/stories.md`: eight findings, and not one of them came from reading the code.

https://github.com/DiegohNY/costlane

---

## Notes for the poster

- The fifth paragraph is entirely limitations, on purpose, and it is the
  longest one. HN is unusually good at finding the thing you left out, and
  being the one to say it first is both honest and cheaper.
- "Why not LiteLLM" will come up, and the honest answer is not a
  differentiator: LiteLLM does far more than this and is the right choice
  for most people. It enforces per-key budgets in the request path too —
  `_virtual_key_max_budget_check` in `proxy/auth/auth_checks.py`, checked at
  auth time against a spend counter. What I built is the small version of
  one thing, with the concurrency tests and the ADRs to show how it is
  enforced. Say that, and do not imply a defect in theirs.
- Do not make any comparative claim that has not been checked against the
  other project's current documentation. An earlier draft of this post said
  the alternatives could only report spend after the fact. That was wrong,
  and HN would have taken about ten minutes to say so.
- Every number above is measured and reproducible from the repo: the
  overhead figures come from the `bench` CI job, the Gemini agreement from
  `docs/provider-verification.md`.
- If asked about scale: nothing here has run in production. Say that.
- The `:latest` story in the last paragraph is checkable — the failed run and
  the fix are both public. Do not soften it into "we have good CI"; the
  specific version is the interesting one.
