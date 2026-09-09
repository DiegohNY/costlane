# costlane

costlane — an LLM gateway that meters tokens, costs and budgets per key.
Drop-in OpenAI-compatible.

**Status: design phase.** No runnable code yet. The design is complete and
under implementation; see [`docs/`](docs/).

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

## Documentation

- [Design document](docs/superpowers/specs/2026-09-09-costlane-design.md) —
  architecture, streaming model, budget concurrency, pricing, API
- [Implementation plan](docs/superpowers/plans/IMPLEMENTATION.md) —
  phased, with progress tracked in-repo

## License

Apache-2.0. See [LICENSE](LICENSE).
