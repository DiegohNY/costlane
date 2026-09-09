package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// charsPerToken is the divisor behind the pre-flight estimate.
//
// It understates real token counts, by 20-30% on code, JSON and non-English
// text, and it understates them consistently rather than in both directions.
// That bias is why the reservation guard band is one-sided.
const charsPerToken = 4

// EstimateInputTokens approximates the tokens in a prompt from its length.
//
// Running an exact tokenizer here would be the wrong trade. It does not
// remove the uncertainty — the provider's own count still differs for tools,
// system prompts and images — and it puts BPE over a large prompt on the
// critical path of the heaviest requests, which are exactly the ones that can
// least afford it. The exact count is available at settle, where it is free.
func EstimateInputTokens(promptBytes int) int64 {
	if promptBytes <= 0 {
		return 0
	}
	return int64(promptBytes/charsPerToken) + 1
}

// EstimateRequest describes what to price before a request runs.
type EstimateRequest struct {
	Model    string
	Provider string
	Tier     Tier
	At       time.Time

	PromptBytes int
	// MaxTokens is the client's own ceiling when it set one. It bounds the
	// output far better than any guess we could make.
	MaxTokens int
	// DefaultMaxTokens applies when the client set no ceiling. Reserving a
	// model's full output capacity instead would refuse a key with modest
	// remaining budget on every request, however small.
	DefaultMaxTokens int

	// GuardFactor lifts an estimate that lands near a context cliff to the
	// far side of it. Crossing OpenAI's threshold reprices the whole
	// request, so an estimate landing just under would under-reserve by
	// 100%.
	GuardFactor float64
}

// Estimate returns the amount to reserve for a request.
//
// It deliberately reserves high. The reservation is released at settle, so
// the only visible cost of caution is a refusal on a borderline request from
// a key whose remaining budget sits between one and two times that request —
// which is correct behaviour for a product that promises not to overspend.
func (t *Table) Estimate(req EstimateRequest) (decimal.Decimal, Result, error) {
	inputTokens := EstimateInputTokens(req.PromptBytes)

	outputTokens := int64(req.MaxTokens)
	if outputTokens <= 0 {
		outputTokens = int64(req.DefaultMaxTokens)
	}

	// Choose the context tier on the guarded input count, then price every
	// kind at that tier.
	guard := req.GuardFactor
	if guard <= 0 {
		guard = TierGuardFactor
	}
	guarded := GuardedTierTotal(inputTokens,
		t.Thresholds(req.Model, req.Provider, req.Tier), guard)

	counts := Counts{
		KindInput:  guarded,
		KindOutput: outputTokens,
	}
	res, err := t.Cost(Request{
		Model: req.Model, Provider: req.Provider, Tier: req.Tier,
		At: req.At, Counts: counts,
	})
	if err != nil {
		return decimal.Zero, Result{}, err
	}
	return res.Cost, res, nil
}
