# Architecture decision records

Seven decisions in this project reversed an earlier one. They are recorded
here because the reversal is the interesting part: the first answer was
reasonable, something was learned, and the second answer is the one the code
implements.

Decisions that were made once and never questioned are not here. They are in
the [design document](../superpowers/specs/2026-09-09-costlane-design.md).

| ADR | Decision | Reversed |
|-----|----------|----------|
| [0001](0001-cancel-not-drain.md) | Cancel the upstream when a client disconnects | Draining for exactness |
| [0002](0002-long-price-format.md) | One row per rate, with its source and date | A compact per-model price map |
| [0003](0003-reserve-in-one-statement.md) | Authenticate, check and reserve in one UPDATE | SELECT the key, then UPDATE the budget |
| [0004](0004-explicit-read-committed.md) | Set READ COMMITTED explicitly on every transaction | Inheriting the server default |
| [0005](0005-no-client-header-passthrough.md) | Forward no client header by default | Relaying the client's headers upstream |
| [0006](0006-partially-priced.md) | Record partially priced separately from unpriced | One `unpriced` boolean |
| [0007](0007-redaction-by-possession.md) | Redact by what this process holds, not by pattern | Pattern matching known key formats |
