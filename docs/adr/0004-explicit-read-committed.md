# 0004 — Set READ COMMITTED explicitly on every budget transaction

Status: accepted. Supersedes inheriting the server default.

## Context

READ COMMITTED is Postgres's default isolation level, and it is the level the
reserve depends on. The first version of the reserve simply began a
transaction and relied on that default.

## Decision

Set the level explicitly, on every transaction that moves budget:

```go
tx, err := db.write.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
```

The correctness argument for the fused reserve
([ADR 0003](0003-reserve-in-one-statement.md)) is specific to this level.
Under READ COMMITTED, a statement that blocks on a row another transaction is
updating re-evaluates its `WHERE` clause against the newly committed value
once the lock is released. That re-evaluation is what makes the second of two
concurrent reserves see the first one's spend.

Under REPEATABLE READ the same situation raises a serialisation failure
instead, and every reserve would need a retry loop that does not exist. So the
reserve is not merely compatible with READ COMMITTED; it is written for it.

That being true, the level is not a default to inherit. It is a premise. A
managed Postgres with a different `default_transaction_isolation`, or a
connection pooler configured by someone else, would change the behaviour of
the budget with nothing in the code to show it. Two lines in a transaction
option are cheaper than that failure mode.

## Alternatives rejected

**Inherit the default.** What was there. Rejected because the default is
someone else's setting, and this code's correctness argument is not.

**Use SERIALIZABLE and retry.** Stronger, and the retry loop it demands sits
on the hottest path in the system, for a guarantee the single-statement
reserve already provides.

**Assert the level at boot.** Catches a misconfigured server, and only that
one: a pooler or a session setting could still change it later. Setting it per
transaction is both the assertion and the fix.

## Consequences

- Tests assert the level from inside the transaction — the reserve and the
  settle both take an `ObserveIsolation` hook — so the premise is verified
  rather than assumed.
- The dependency is documented where it lives: in a comment on the reserve
  statement, next to the predicate it protects.
