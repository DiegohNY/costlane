# 0005 — Forward no client header upstream by default

Status: accepted. Supersedes relaying the client's headers.

## Context

A transparent proxy relays what it receives. The first version forwarded the
client's headers to the provider, on the reasoning that a gateway should stay
out of the way — a client setting `OpenAI-Beta` or some custom header would
then just work.

## Decision

Forward nothing, except an explicit allowlist that is empty in production.

The client's `Authorization` header is a costlane virtual key. Relaying it
sends that credential to OpenAI, Anthropic and Google, where it lands in their
logs, is out of our control, and grants spending rights on this gateway to
anyone who reads them.

Stripping only `Authorization` and forwarding the rest was considered and
rejected as the wrong shape: it is a denylist, and a denylist of headers is a
list someone has to keep complete forever. `Cookie`, `X-Forwarded-For` and
whatever a future client invents all carry the same class of problem, and a
header that lets a caller set options on *our* provider account is a second
class of problem again.

An allowlist that is empty by default fails closed. Adding to it is a
deliberate act with a reviewer attached.

## Alternatives rejected

**Forward everything.** The original. Leaks the virtual key to three third
parties.

**Forward everything except `Authorization`.** A denylist: wrong by default,
and wrong again the next time a header matters.

**Forward a fixed allowlist of "safe" headers.** Nothing has needed one yet.
The mechanism exists — a prefix allowlist, used by the tests to drive the fake
provider — so the day something does need it, the change is a configuration
value rather than a redesign.

## Consequences

- A client cannot pass provider-specific headers through the gateway. Nobody
  has asked to; when someone does, it will be an allowlist entry.
- Request *bodies* still reach OpenAI-compatible providers byte for byte. This
  ADR is about headers, which carry credentials, not about bodies, which carry
  the request.
