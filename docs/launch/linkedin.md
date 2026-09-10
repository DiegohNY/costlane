# LinkedIn draft

**Not published. For review.** 247 words.

---

Before its first release, my LLM gateway metered every streamed OpenAI request at zero tokens. An end-to-end test caught it; a user never would have.

The response was correct. The stream was correct. The accounting said nothing had been spent.

Two features, each working. To meter a stream you need the provider's own token counts, so the gateway injects `stream_options.include_usage` into every streamed request. If the client never asked for that chunk, the gateway strips it back out on the way past. Injection worked. Stripping worked. Nothing read the chunk in between.

It is the worst shape a bug can have in a billing system: silent, total, invisible from outside. Nobody complains. You find out at the end of the month.

What I built is Costlane: a proxy that meters cost per virtual key and refuses a request that would take that key past its budget, before the provider is called rather than after. Open source, Apache-2.0.

The test that caught it is now a required check in CI, which is the only part of this worth copying.

What I took from it: the bugs that matter in accounting systems are not the ones that produce a wrong number. They are the ones that produce no number, because a wrong number gets argued about and a missing one gets assumed to be zero. Every place the gateway cannot compute a cost now records NULL, never 0 — unknown and free should never be spelled the same way.

github.com/DiegohNY/costlane

---

## Notes for the poster

- No hashtags, no "excited to announce", no emoji. The bug is the hook.
- The last paragraph is the actual point; the product is the middle.
- If someone asks what else it does, the README answers better than a
  comment will. Link, do not paste.
