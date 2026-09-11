# LinkedIn draft

**Not published. For review.** Current as of v0.2.1. 246 words.

---

Before its first release, my LLM gateway metered every streamed OpenAI request at zero tokens. An end-to-end test caught it; a user never would have.

The response was correct. The stream was correct. The accounting said nothing had been spent.

Two features, each working. To meter a stream you need the provider's token counts, so the gateway injects `stream_options.include_usage` and strips the resulting chunk back out if the client never asked for it. Injection worked. Stripping worked. Nothing read the chunk in between.

It is the worst shape a bug can have in a billing system: silent, total, invisible from outside. You find out at the end of the month.

What I built is Costlane: a proxy that meters cost per virtual key and refuses a request that would exceed its budget. Open source, Apache-2.0.

The test that caught it is now required in CI. What I took from it: the bugs that matter in accounting are not the ones that produce a wrong number, but the ones that produce no number — a wrong number gets argued about, a missing one gets assumed to be zero. Every place the gateway cannot compute a cost now records NULL, never 0.

The follow-up: I checked the meter against Google's own telemetry. Three readings of the same requests — mine, the provider's response, their metrics — agreeing to the token. The only evidence I have that it is right came from a system with no idea I exist.

github.com/DiegohNY/costlane

---

## Notes for the poster

- No hashtags, no "excited to announce", no emoji. The bug is the hook.
- The NULL-versus-zero paragraph is the actual point; the product is the
  middle, and the telemetry follow-up closes on the only outside evidence
  the thing is right.
- If someone asks what else it does, the README answers better than a
  comment will. Link, do not paste.
