package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoUsage reports a payload that carries no usage block. It is an error
// rather than an empty count: a response we could not read is not a response
// that consumed nothing.
var ErrNoUsage = errors.New("pricing: response carries no usage")

// Normalised is a provider's usage translated into our billing classes.
//
// Degraded and ParseErrors travel together with the counts because a figure
// we had to reconstruct is worth less than one the provider stated, and the
// usage record should say which it got.
type Normalised struct {
	Counts      Counts
	Degraded    bool
	ParseErrors int
}

func (n *Normalised) degrade(reason string) {
	n.Degraded = true
	n.ParseErrors++
	_ = reason // retained for the caller's structured log
}

// clampNegative replaces any negative count with zero and degrades the
// result.
//
// A token count below zero is not something a provider should ever send, and
// that is exactly why it is worth handling here rather than trusting it not
// to happen. Downstream, Table.Cost refuses a negative count with an error,
// and an error there is swallowed into a zero cost — so an absurd figure
// upstream would become a free request rather than a loud one. Clamping and
// degrading keeps the request accounted for and marks the figure as one
// nobody should rely on.
//
// Found by the Gemini stream fuzz target on its first run, against every
// provider rather than one.
func (n *Normalised) clampNegative() {
	for kind, count := range n.Counts {
		if count < 0 {
			n.degrade("provider reported a negative " + string(kind) + " count")
			n.Counts[kind] = 0
		}
	}
}

// NormaliseOpenAI translates a Chat Completions or Responses usage block.
//
// Cached and cache-write tokens are subsets of the prompt total, so they are
// subtracted to leave the plain input. Billing them without subtracting
// would charge every cached token twice: once at the full rate and once at
// the cached one.
//
// Reasoning tokens are a subset of the completion total and are billed at
// the output rate, so they are recorded but not added.
func NormaliseOpenAI(payload []byte) (Normalised, error) {
	var body struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			// The Responses API names for the same quantities.
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`

			PromptDetails *struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			InputDetails *struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`

			CompletionDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			OutputDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return Normalised{}, fmt.Errorf("pricing: parsing openai usage: %w", err)
	}
	if body.Usage == nil {
		return Normalised{}, ErrNoUsage
	}
	u := body.Usage

	promptTotal := u.PromptTokens
	if promptTotal == 0 {
		promptTotal = u.InputTokens
	}
	outputTotal := u.CompletionTokens
	if outputTotal == 0 {
		outputTotal = u.OutputTokens
	}

	var cached, cacheWrite int64
	if d := u.PromptDetails; d != nil {
		cached, cacheWrite = d.CachedTokens, d.CacheWriteTokens
	} else if d := u.InputDetails; d != nil {
		cached, cacheWrite = d.CachedTokens, d.CacheWriteTokens
	}

	var reasoning int64
	if d := u.CompletionDetails; d != nil {
		reasoning = d.ReasoningTokens
	} else if d := u.OutputDetails; d != nil {
		reasoning = d.ReasoningTokens
	}

	out := Normalised{Counts: Counts{}}

	plain := promptTotal - cached - cacheWrite
	if plain < 0 {
		// The provider reported more cached tokens than prompt tokens.
		// Clamping silently would hide the inconsistency behind a
		// plausible number.
		out.degrade("cached and cache-write tokens exceed the prompt total")
		plain = 0
	}

	out.Counts[KindInput] = plain
	out.Counts[KindCachedRead] = cached
	// OpenAI does not distinguish cache TTLs; its writes map to the short one.
	out.Counts[KindCacheWrite5m] = cacheWrite
	out.Counts[KindOutput] = outputTotal
	out.Counts[KindReasoning] = reasoning

	if reasoning > outputTotal {
		out.degrade("reasoning tokens exceed the output total")
	}
	out.clampNegative()
	return out, nil
}

// NormaliseAnthropic translates a Messages API usage block.
//
// Anthropic's cache fields are separate from input_tokens rather than
// subsets of it, so they are added: the documented identity is
// total_input = cache_read + cache_creation + input. This is the opposite of
// OpenAI, and getting it backwards understates every cached request.
//
// Cache writes carry a TTL split that the flat total does not express, and
// the two TTLs are priced 1.6x apart. The nested breakdown is preferred; if
// it does not reconcile with the flat total, the result is degraded rather
// than guessed.
//
// Thinking is billed as output and already counted inside output_tokens.
func NormaliseAnthropic(payload []byte) (Normalised, error) {
	var body struct {
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheCreation            *struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return Normalised{}, fmt.Errorf("pricing: parsing anthropic usage: %w", err)
	}
	if body.Usage == nil {
		return Normalised{}, ErrNoUsage
	}
	u := body.Usage

	out := Normalised{Counts: Counts{}}
	out.Counts[KindInput] = u.InputTokens
	out.Counts[KindCachedRead] = u.CacheReadInputTokens
	out.Counts[KindOutput] = u.OutputTokens

	switch {
	case u.CacheCreation != nil:
		split := u.CacheCreation.Ephemeral5m + u.CacheCreation.Ephemeral1h
		if u.CacheCreationInputTokens != 0 && split != u.CacheCreationInputTokens {
			// Preserve the flat total, which is the figure the provider
			// stands behind, and record that the TTL attribution is
			// unreliable. Dropping the difference would lose real spend.
			out.degrade("the cache_creation TTL split does not sum to the flat total")
			out.Counts[KindCacheWrite5m] = u.CacheCreationInputTokens
			out.Counts[KindCacheWrite1h] = 0
			break
		}
		out.Counts[KindCacheWrite5m] = u.CacheCreation.Ephemeral5m
		out.Counts[KindCacheWrite1h] = u.CacheCreation.Ephemeral1h
	case u.CacheCreationInputTokens > 0:
		// No breakdown at all: attribute to the short TTL, which is the
		// default, and say the attribution was assumed.
		out.degrade("cache writes reported without a TTL breakdown")
		out.Counts[KindCacheWrite5m] = u.CacheCreationInputTokens
	}

	out.clampNegative()
	return out, nil
}

// NormaliseGoogle translates a usageMetadata block.
//
// Google is the only provider that goes both ways: cachedContentTokenCount is
// a subset of promptTokenCount and must be subtracted, while
// thoughtsTokenCount is separate from candidatesTokenCount and must be added.
// Getting either direction wrong is a silent mis-bill, and they fail in
// opposite directions.
func NormaliseGoogle(payload []byte) (Normalised, error) {
	var body struct {
		Usage *struct {
			PromptTokenCount        int64 `json:"promptTokenCount"`
			CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
			CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
			ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
			TotalTokenCount         int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return Normalised{}, fmt.Errorf("pricing: parsing google usage: %w", err)
	}
	if body.Usage == nil {
		return Normalised{}, ErrNoUsage
	}
	u := body.Usage

	out := Normalised{Counts: Counts{}}

	plain := u.PromptTokenCount - u.CachedContentTokenCount
	if plain < 0 {
		out.degrade("cached content exceeds the prompt total")
		plain = 0
	}

	out.Counts[KindInput] = plain
	out.Counts[KindCachedRead] = u.CachedContentTokenCount
	// Thoughts are billed as output and reported separately, so they are
	// added into the output count and also recorded on their own.
	out.Counts[KindOutput] = u.CandidatesTokenCount + u.ThoughtsTokenCount
	out.Counts[KindReasoning] = u.ThoughtsTokenCount

	out.clampNegative()
	return out, nil
}
