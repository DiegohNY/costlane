# Security

## Threat model, in ten lines

1. costlane sits between an application and an LLM provider, so it **sees every
   prompt and every completion** that passes through it.
2. It **stores** them only when `COSTLANE_LOG_PROMPTS` is on, which requires
   `COSTLANE_PROMPT_RETENTION` to be set; the process refuses to start
   otherwise.
3. It **protects the provider credentials**: they live in the gateway's
   environment, never reach a client, and are stripped from anything relayed
   outwards by value, not by pattern
   ([ADR 0007](docs/adr/0007-redaction-by-possession.md)).
4. It **protects the budget**: a virtual key cannot spend past its limit, and
   the limit holds under concurrency
   ([ADR 0003](docs/adr/0003-reserve-in-one-statement.md)).
5. It **isolates keys from each other**: a virtual key reads its own usage and
   nothing else; the master credential is the only way to see across keys.
6. It **does not forward client headers upstream**, so a caller cannot set
   options on the operator's provider account
   ([ADR 0005](docs/adr/0005-no-client-header-passthrough.md)).
7. It is **not a WAF**: it does not inspect, filter or sanitise prompt content,
   and it will happily proxy a prompt injection to a model.
8. It **does not authenticate end users** — a virtual key identifies an
   application, not a person — and it does not rate limit.
9. It assumes the **database and the gateway are on a trusted network**; the
   admin API is protected by one shared master credential and nothing else.
10. A compromise of the gateway host is a compromise of the provider keys and
    of any prompt in flight. Treat it as a credential-bearing service.

## Reporting a vulnerability

Report privately through GitHub's security advisories:
**[Report a vulnerability](https://github.com/DiegohNY/costlane/security/advisories/new)**.

Please do not open a public issue for anything that could be exploited before
it is fixed. Expect an acknowledgement within a week. There is no bounty; this
is a personal project, and the thanks are in the changelog.

## What is enforced in CI

Every pull request blocks on these, along with the usual tests:

- **`govulncheck`** — a known vulnerability in a dependency fails the build.
- **A credential-leak test** — it drives every failure path (upstream errors,
  timeouts, malformed responses, budget refusals) and then searches logs,
  response bodies, metrics and database rows for every credential the process
  holds.
- **Fuzzing** of the two parsers that read third-party input: the SSE frame
  reader and the Anthropic stream translator.

## Operating notes

- Generate the master key with `openssl rand -hex 32`. Anything under 32
  characters is refused at boot.
- The `docker-compose.yml` in this repository is a demo. Its master key is
  published in the file, and its Postgres has a known password on no exposed
  port. Do not deploy it.
- Prompt capture is a debugging aid. Turn it on with a retention period you
  would be comfortable explaining to the customer whose data it is, and turn
  it off afterwards.
