# Launch material

Internal drafts. **Nothing here has been published**, and nothing here should
be published without a read-through first.

- [show-hn.md](show-hn.md) — title and two paragraphs, Show HN style.
- [linkedin.md](linkedin.md) — a technical post that opens with a bug, not an
  announcement.
- [reddit-golang.md](reddit-golang.md) — for r/golang: the SSE relay, the
  fused reserve, the isolation level, the fuzzing.

Three rules the drafts follow, so an edit does not undo them:

1. **Every number is measured.** Overhead figures come from the `bench` CI
   job, the Gemini agreement from [provider
   verification](../provider-verification.md). If a figure cannot be pointed
   at a run, it does not go in.
2. **Limitations are stated by the author, not discovered by the reader.**
   The Show HN draft spends its second paragraph on them.
3. **No hype vocabulary.** No "excited to announce", no "blazing fast", no
   claim about production use, because there is none.
4. **No comparative claim about another project without re-reading its
   documentation in the 48 hours before posting.** An earlier draft of the
   Show HN post said the alternatives could only report spend after the
   fact. LiteLLM enforces per-key budgets in the request path and has for
   some time — the claim was simply false, and the correction cost nothing
   only because it was caught before publication. A comparison that is
   wrong takes the whole post down with it.

The raw material for all three is [stories.md](../stories.md).
