# Show HN draft

**Not published. For review.**

---

**Title:**

> Show HN: Costlane – an LLM gateway that stops a key at its budget, in the request path

*(78 characters. HN truncates around 80.)*

---

**Body:**

Costlane is an OpenAI-compatible proxy that sits between an application and OpenAI, Anthropic or Google, meters every request exactly, and refuses one that would take a virtual key past its spending limit. The refusal happens before the provider is called, so the budget is a cap rather than a report: a key that is out of money gets a 402, not an alert tomorrow morning. That is the part I could not find elsewhere. The tools I looked at read usage after the fact, which is useful for attribution and useless for control — by the time a dashboard shows the overspend, the tokens are bought. Making it a cap rather than a chart is mostly a database problem: authentication, the model allowlist, the monthly window rotation and the limit check are all predicates inside the single UPDATE that takes the reservation, so there is no window between checking and writing, and a thousand parallel requests on one key cannot overspend it. Streaming is metered from the provider's own usage figures read as the chunks pass, not from a tokeniser guessing at them. Measured overhead against a local fake provider is 1.96 ms at p50 on a GitHub runner, almost all of it Postgres; per streamed chunk it is 13 microseconds.

What it does not do, since that matters more than what it does: the cancel-on-disconnect default is documented provider behaviour that I have not measured on any live API — I verified token counting against Gemini and it agrees to the token, but the two providers I would most want to check needed credentials I did not have, so the README says so and offers `disconnect_policy: drain` to anyone who needs certainty rather than a default. Gemini is non-streaming only. There is no Vertex or Bedrock, one master credential and no user accounts, no rate limiting, no caching, no failover, no UI. It is v0.1.0 and I have run it against real providers for exactly one check. `docker compose up` gives you the whole thing with a fake provider and no API keys, and the seven decisions that reversed an earlier one are written up as ADRs, including the two I got wrong first.

https://github.com/DiegohNY/costlane

---

## Notes for the poster

- The second paragraph leads with limitations on purpose. HN is unusually
  good at finding the thing you left out, and being the one to say it first
  is both honest and cheaper.
- Do not answer "why not LiteLLM/Helicone/Langfuse" defensively if it comes
  up. The accurate answer is short: they are not in the request path, so
  they can report spend but cannot cap it. That is a design choice, not a
  defect, and it suits a different problem.
- Every number above is measured and reproducible from the repo: the
  overhead figures come from the `bench` CI job, the Gemini agreement from
  `docs/provider-verification.md`.
- If asked about scale: nothing here has run in production. Say that.
