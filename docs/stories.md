# Stories

Internal notes. Six bugs, what found each one, and what changed afterwards.
Written while the details were fresh, because in a month reconstructing them
from diffs would take a day.

The pattern worth noticing: not one of these was found by reading the code.
Every one was found by a mechanism that ran the code and disagreed with it.

---

## 1. The .gitignore that hid `cmd/`

**Found by:** the Docker build in CI, and nothing else.

The binary patterns in `.gitignore` were written unanchored:

```gitignore
# Binaries
costlane
fakeprovider
```

Git applies an unanchored pattern at *every* level, so those two lines matched
the directories `cmd/costlane` and `cmd/fakeprovider` as well as the compiled
binaries they were meant to match. The packages built perfectly on the machine
that wrote them — the files were there in the working copy — and never entered
a single commit.

Local `go build` was green. `go test` was green. The only step that noticed was
`docker build`, because a Docker build context contains tracked files and
nothing else, and it failed several layers down with a missing directory inside
a container.

**Fixed by** anchoring both patterns to the repository root (`/costlane`), and
by a CI step that fails when any Go source file is untracked:

```bash
untracked=$(git ls-files --others --exclude-standard -- '*.go')
```

The lesson is not about `.gitignore`. It is that the failure surfaced three
layers away from its cause, and the fix was to make it announce itself
directly. That step has never fired since. It cost four lines.

---

## 2. Ten expected successes, or two hundred

**Found by:** deliberately breaking the code to see whether the tests noticed.

The budget concurrency tests were the most important tests in the project — a
thousand parallel requests against one key that must not overspend — and a test
that passes proves nothing about a test that would fail.

So each was run against sabotaged implementations. Removing the budget
predicate from the reserve turned **10 expected successes into 200**: every
request got through, the limit meant nothing, and the test said so loudly.
Releasing the reserved amount unconditionally — a bug found earlier during
design review — failed the settle-versus-reaper race. Leaving the reservation
state non-terminal failed it too.

All three sabotages were caught. The concurrency tests were then run eight
times consecutively to confirm they were deterministic rather than lucky.

**What changed:** nothing in the implementation. The value was entirely in
knowing the tests had teeth. Mutating the code under a test suite takes
minutes and is the only way to distinguish a test from a decoration.

---

## 3. OpenAI streaming counted no tokens at all

**Found by:** a test written before the streaming implementation existed.

For its own accounting the gateway injects `stream_options.include_usage` into
every streamed request, so the provider emits a final chunk carrying the token
counts. If the client did not ask for that chunk, the gateway strips it again
on the way past, so the client receives exactly the stream it would have
received without us.

Both halves worked. Nothing read the chunk in between. Every streamed OpenAI
request was metered at zero, and every one of them cost real money.

This is the worst class of bug this product can have: silent, total, and
invisible from the outside. The response was correct. The stream was correct.
The invoice would have been a surprise at the end of the month.

**Fixed by** an `ObserveUsage` hook on the pump, which reads the payload as the
chunk goes past, whether or not it is then stripped.

The same commit surfaced a second one: stream accounting ran on the *request*
context, so a client that disconnected mid-stream left no usage record at all.
Accounting now runs on a context detached from the caller's — the tokens were
bought whether or not anyone is still listening.

---

## 4. The unpriced model that reserved zero

**Found by:** a test asking what happens to a model the price table has never
heard of.

An unpriced model estimates at zero. A reservation for zero succeeds against
any budget, including one with a single cent left. The request went upstream,
spent real money, and was recorded with a null cost — untracked spend passing
through a gateway that exists to prevent exactly that.

The refusal could not be moved to the settle, because by then the tokens are
bought. It had to happen *before* the reserve: once a zero reservation has
succeeded there is nothing left to refuse.

**Fixed by** checking, before reserving, whether the key carries a budget at
all. Unpriced model plus budgeted key is a 400 that says so by name. Unpriced
model on a key with no limit proceeds and is recorded with a NULL cost —
unknown, never zero.

The general shape: a default of zero is dangerous in a system where zero is
also a legitimate value. "Unknown" needs its own representation, and NULL is
it. The same reasoning produced
[ADR 0006](adr/0006-partially-priced.md).

---

## 5. The canary that was not in any known format

**Found by:** the credential-leak test, on the day it was written.

Redaction started as a set of patterns for the credential formats providers use
today — `sk-` prefixes and their relatives. It worked on every provider key it
was shown.

The leak test plants distinct canary strings for each secret the process holds:
the provider key, the master key, the database password, an upstream error
body, a prompt. Then it drives every failure path the gateway has — upstream
errors, timeouts, malformed responses, budget refusals — and greps logs,
response bodies, metrics and database rows for all of them.

The canaries that were not shaped like a provider key went straight through to
the client. Of course they did: a pattern matches what a credential *looks
like*, and the master key and the database password look like nothing in
particular. So would any provider's next key format, or a self-hosted
deployment's token.

**Fixed by** redaction by possession: at boot the process builds a redactor
holding its own secrets and removes those literal values from anything leaving
it. Patterns stayed, because they catch the one thing possession cannot — a
credential that is not ours, pasted by a user into a prompt.
[ADR 0007](adr/0007-redaction-by-possession.md).

The test is an integration test on purpose. Credentials escape through the
seams between components, and a unit test never crosses a seam.

---

## 6. The cancelled context that lost the record

**Found by:** the same class of thinking, twice, in two different places.

A settle runs at the end of a request. By then the client may have
disconnected, which cancels the request context. Using that context for the
settle aborts the transaction: the reservation stays pending until the reaper
sweeps it, and until then real spend is missing from the budget.

The identical mistake appeared on the usage-record path. A client that
disconnected after the provider replied had still consumed tokens, and its
record was cancelled on the way to the database.

**Fixed by** `context.WithoutCancel` plus a fresh timeout on both paths.
Accounting outlives the request it accounts for, by construction.

This one has a rule of thumb attached: **a request context is a permission to
stop doing work for the client, not a permission to stop recording what was
already spent.** Anywhere the two diverge, the accounting takes a detached
context.

---

## What this adds up to

Six bugs, five mechanisms:

| Bug | What caught it |
|-----|----------------|
| `.gitignore` hid `cmd/` | Docker build in CI |
| Tests with no teeth | Deliberate sabotage of the implementation |
| Streaming counted nothing | A test written before the code |
| Unpriced model reserved zero | A test asking about an absent price |
| Out-of-format canary | An integration test that greps every seam |
| Cancelled context lost records | Reasoning about lifetimes, then a test |

Not one of them was found by review. Two were found by a build step that had
nothing to do with the bug. Three were found because a test existed before the
code did.
