# costlane

costlane — an LLM gateway that meters tokens, costs and budgets per key.
Drop-in OpenAI-compatible.

**Status: under construction.** The gateway proxies, meters and enforces
budgets for non-streaming requests today. Streaming, the read API and the
published benchmarks are still to come; see the
[implementation plan](docs/superpowers/plans/IMPLEMENTATION.md).

## What it will do

A reverse proxy between applications and LLM providers (OpenAI, Anthropic,
Google). It speaks the OpenAI API dialect, so a client switches to it by
changing `base_url` and keeps its existing SDK.

Version 1 scope:

1. Proxies chat completions, including SSE streaming, with minimal overhead.
2. Accounts for tokens and cost exactly, on every request.
3. Issues virtual keys — one per team, feature, or customer — from which
   spend attribution follows.
4. Enforces per-key budgets with a hard stop that stays correct under
   concurrency.
5. Exposes a read API for querying spend and usage.

Out of scope for v1: semantic caching, multi-provider failover, evaluation
suites, and any frontend.

## Behaviour worth knowing about

**Request bodies reach OpenAI-compatible providers byte for byte.** costlane
does not parse and re-serialise them, so a parameter it has never heard of
still arrives intact.

**Parameters with no equivalent are refused, not dropped.** Asking Anthropic
for `logprobs`, or Gemini for `seed`, returns 400 naming the parameter. A
client that receives a response cannot tell a silently discarded field from
one the model ignored, so costlane does not create that ambiguity.

**`max_tokens` is injected for Anthropic only.** The Messages API rejects a
request without it, so costlane supplies `COSTLANE_DEFAULT_MAX_TOKENS`
(4096 by default) and reports the fact in `X-Costlane-Injected-Max-Tokens`.
This is the single deliberate exception to leaving requests alone, and it
uses the same figure as the budget reservation, so the amount reserved and
the ceiling actually applied agree. No other provider gets an injected
ceiling.

**Crossing a context threshold reprices the whole request.** OpenAI charges
double for input above 272,000 tokens — for every token, not only those past
the boundary. A reservation whose estimate lands within a quarter of that
threshold reserves at the higher rate, so a request near the edge is refused
rather than under-reserved.

**An unpriced model is refused on a budgeted key.** Cost that cannot be
computed cannot be capped, and letting it through would be untracked spend.
On a key with no limit the request proceeds and is recorded with a null
cost — unknown, never zero.

**Every response carries its own cost.**

```
X-Costlane-Cost-Usd: 0.002
X-Costlane-Budget-Remaining-Usd: 4.998
X-Costlane-Request-Id: 0e4253ac-a941-4826-be5c-16fc3b9497cb
```

## Documentation

- [Design document](docs/superpowers/specs/2026-09-09-costlane-design.md) —
  architecture, streaming model, budget concurrency, pricing, API
- [Implementation plan](docs/superpowers/plans/IMPLEMENTATION.md) —
  phased, with progress tracked in-repo

## License

Apache-2.0. See [LICENSE](LICENSE).
