# 0003 — Authenticate, check and reserve in one statement

Status: accepted. Supersedes SELECT-then-UPDATE.

## Context

Admitting a request means answering several questions: does this key exist, is
it revoked, is this model allowed, has the spending window rolled over, and is
there room in the budget for what this request might cost?

The natural implementation reads the key, reads the budget, decides, and then
writes the reservation. That is what was written first, and it works in a
single-threaded test.

## Decision

Fuse all of it into one `UPDATE ... FROM ... WHERE ... RETURNING`, inside one
transaction. Every check is a predicate in the `WHERE` clause; the window
rotation is a `CASE` in the `SET` clause; the reservation row is the only
other statement.

Two reasons, in order of importance.

**Correctness.** Between a `SELECT` that says there is room and an `UPDATE`
that takes it, another request can take the same room. Under concurrency the
budget can be exceeded by however many requests fit in that window. Moving the
predicate inside the `UPDATE` closes it: when two transactions target the same
row, the second blocks and then re-evaluates its predicate against the
committed value. See [ADR 0004](0004-explicit-read-committed.md), on which
that behaviour depends.

**Latency.** It leaves exactly one database round trip between an accepted
request and the provider call. There is a test that counts the statements a
request issues and fails if anything is added in front of the reserve.

## Alternatives rejected

**SELECT then UPDATE.** The original. Rejected: it exceeds the budget under
concurrency, which is the one thing the product must not do.

**SELECT ... FOR UPDATE, then decide, then write.** Correct, and two round
trips holding a row lock instead of one. The fused statement gets the same
guarantee for less.

**Advisory locks per key.** Correct and slower, with a lock namespace to
manage. The row already serialises what needs serialising.

**Optimistic concurrency with a retry loop.** More code, more variance under
contention, and no better outcome than letting Postgres block.

## Consequences

- A refusal returns no rows, which is ambiguous — revoked key, disallowed
  model, exhausted budget. A second query runs *only* on that path to say
  which. The common path never pays for it.
- The statement is long and dense, and a reader has to hold the whole thing in
  their head to see why it is correct. That cost is paid once, in a commented
  constant, rather than at every call site.
- Correctness under concurrency is tested by racing requests at one key and
  asserting the limit is never crossed.
