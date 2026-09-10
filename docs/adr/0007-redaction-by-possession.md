# 0007 — Redact by what this process holds, not by what a key looks like

Status: accepted. Supersedes pattern-based redaction.

## Context

Credentials escape through the seams of a system: a log line, an error body
relayed from a provider, a metric label, a database column. The first defence
written was a set of regular expressions for the known key formats — `sk-`
prefixes, and the rest.

## Decision

Keep the patterns, and add the thing that actually works. At boot the process
builds a redactor holding its own secrets — the master key, each provider key,
the database password — and every byte sequence leaving the process is
searched for those literal values.

A pattern matches what a credential is *shaped* like. Possession matches what
this process actually *has*. The two fail in opposite directions, and only one
of them fails safe:

- A provider changes its key format, or a self-hosted deployment uses a token
  that looks like nothing in particular. The pattern misses it. Possession
  does not.
- A provider echoes our own key back inside an error body — which happens —
  and that body is relayed to the client. Possession catches it regardless of
  format.

The patterns stay, because they catch one thing possession cannot: a
credential that is *not ours*, pasted by a user into a prompt.

The complementary half is the `Secret` type, which implements every interface
Go reaches for when rendering a value — `Stringer`, `Formatter`,
`json.Marshaler`, `slog.LogValuer` — so a credential printed by accident
renders as `[redacted]`. Reading the real value requires calling `Expose`,
which is greppable and obvious in review.

## Alternatives rejected

**Patterns only.** The original. Misses any format not anticipated, which is
every format that has not shipped yet.

**Possession only.** Misses a third party's credential appearing in a prompt
or in an error.

**Redact at the logging layer only.** Credentials also leave through response
bodies and database columns. The test that guards this drives every failure
path and then searches all of them, which is why it is an integration test
rather than a unit test.

## Consequences

- A short secret is not registered as a literal: below eight characters the
  false positives would corrupt ordinary output. Configuration enforces a
  minimum length on the master key.
- Redaction costs a scan of every relayed error body. Error bodies are small
  and rare, and they are exactly where the credential would otherwise be.
- The guarantee is enforced by a dedicated CI job that exercises the failure
  paths and then greps logs, response bodies, metrics and database rows for
  every credential the process holds.
