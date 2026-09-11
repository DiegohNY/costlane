# r/golang draft

**Not published. For review.** Current as of v0.2.1.

---

**Title:**

> Costlane: an LLM gateway in Go — SSE relay with no buffer, budget enforcement in one UPDATE

---

**Body:**

I wrote an OpenAI-compatible proxy that meters token spend per virtual key and refuses requests that would exceed a budget. Five things in it were more interesting to build than I expected, and none of them are really LLM problems.

**A streaming relay with nothing between read and write.** One goroutine reads an SSE frame, counts its tokens and writes it, in the same loop iteration. There is no channel between reading and writing, because a channel is a buffer and the requirement is that a chunk reaches the client the moment it exists. Two things this cost me. First, every `http.ResponseWriter` wrapper has to implement `Unwrap`, or `http.ResponseController` cannot reach the `Flusher` underneath — every chunk is then silently buffered and the whole stream arrives in one piece, which no test that only inspects the final body will ever notice. Second, no `WriteTimeout` on the `http.Server`: it bounds the entire response and would cut every long stream short. Each write gets its own deadline instead. Client departure is noticed from the request context between chunks rather than by waiting for a write to fail, so cancellation happens within one chunk. Measured overhead per chunk is 13 µs.

**Budget enforcement as a single statement.** The obvious implementation reads the key, reads the budget, decides, then writes a reservation. That exceeds the limit under concurrency: between the SELECT that says there is room and the UPDATE that takes it, other requests take the same room. So authentication, the model allowlist, the monthly window rotation and the limit check are all predicates inside one `UPDATE ... FROM ... WHERE ... RETURNING`. A refusal is the statement matching zero rows; a second query runs only then, to say which of several reasons it was.

**READ COMMITTED, set explicitly, as a load-bearing choice.** That fused UPDATE is correct because of one specific behaviour: under READ COMMITTED, a statement that blocks on a row another transaction is updating re-evaluates its `WHERE` clause against the newly committed value when the lock is released. That is what makes the second of two concurrent reserves see the first one's spend. Under REPEATABLE READ the same situation raises a serialisation failure and every reserve would need a retry loop. So the level is not a default worth inheriting from whatever a managed Postgres or a pooler happens to be configured with — every budget transaction sets it, and a test asserts the level from inside the transaction.

**Fuzzing the parsers that read third-party input.** Three targets now — the SSE frame reader and the Anthropic and Gemini stream translators — all parsing bytes from someone else's server, which is what fuzzing exists for. The Gemini one earned its place on its first run: a negative token count from a provider went straight into the meter, where `Table.Cost` refuses it with an error that callers log and swallow into a zero cost. An absurd figure upstream becoming a free request. 752,000 executions on the frame reader and 2.9 million on the Anthropic translator (from the F6 run; every PR fuzzes 30 s per target, nightly longer).

**Writing a protocol translator against captured bytes rather than docs.** For Gemini's streaming endpoint, the documentation describes two dialects — `content.start`/`content.delta` events, and a newer Interactions API with usage under `total_output_tokens`. The endpoint sends neither: it streams whole `GenerateContentResponse` objects with usage under `candidatesTokenCount` and `thoughtsTokenCount`. I found that out by capturing real responses into `testdata/` before writing a line of the translator, and the fixtures are the spec — where they and the code disagree, the code is wrong. Usage also rides cumulatively on every chunk rather than arriving once at the end, so a translator that summed would have multiplied the count by the number of chunks.

To check the concurrency tests actually tested something, I ran each against deliberately broken implementations. Removing the budget predicate turned 10 expected successes into 200. Two other sabotages failed the settle-versus-reaper race. Mutating the code under a test suite takes minutes and is the only way to tell a test from a decoration.

The one I would actually recommend copying, though, is smaller. The release workflow pushes the version tag, boots *that digest* against a real Postgres, and only re-points `:latest` once it answers. Its first run failed for an unrelated reason — I had forgotten a required env var in the job — and the text of its own failure showed that the provider I had just spent a release adding was never registered in the router, so every request to it had been answering 404 for two releases. Six kinds of test had missed it, because nothing crossed that seam: `buildRouter` is reached only from `main`, and the one end-to-end test through it used a different provider. `:latest` never moved. Running the real artifact the real way catches things you did not think to assert.

Design decisions that reversed an earlier one are written up as ADRs — cancel vs drain, the fused reserve, the explicit isolation level, and five others: https://github.com/DiegohNY/costlane/tree/main/docs/adr

Repo: https://github.com/DiegohNY/costlane — `docker compose up` runs the whole thing against a fake provider with no API keys.

---

## Notes for the poster

- r/golang responds to specifics and punishes marketing. The product is
  mentioned twice and explained once.
- `Unwrap` on ResponseWriter wrappers is the detail most likely to start a
  useful thread; it catches people out constantly.
- Be ready for "why not just use a channel with a buffer of 1" — the answer
  is that it does not help: the write still has to complete before the next
  read, and the buffer only hides when it does not.
- Every figure is measured and reproducible from the repo.
- The captured-fixtures point is the one most likely to be argued with
  ("the docs are the contract"). The answer is short: the endpoint is the
  contract, the docs described a different one, and the capture is checked
  into the repository for anyone to verify.
