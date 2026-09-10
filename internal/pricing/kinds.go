package pricing

// Kind is a billing class. Providers keep inventing these — a cache-write
// charge here, a second cache TTL there — so they are values rather than
// struct fields, and a new one costs a constant plus a row.
type Kind string

// The billing classes the supported providers publish rates for. Audio is
// declared but not yet seeded.
const (
	KindInput        Kind = "input"
	KindOutput       Kind = "output"
	KindCachedRead   Kind = "cached_read"
	KindCacheWrite5m Kind = "cache_write_5m"
	KindCacheWrite1h Kind = "cache_write_1h"
	KindReasoning    Kind = "reasoning"
	KindAudioInput   Kind = "audio_input"
	KindAudioOutput  Kind = "audio_output"
)

// AllKinds is the closed set mirrored by the database CHECK constraint.
var AllKinds = []Kind{
	KindInput, KindOutput, KindCachedRead,
	KindCacheWrite5m, KindCacheWrite1h,
	KindReasoning, KindAudioInput, KindAudioOutput,
}

// Valid reports whether k is a known billing class.
func (k Kind) Valid() bool {
	for _, known := range AllKinds {
		if k == known {
			return true
		}
	}
	return false
}

// Tier is a provider service tier. Rates differ across tiers, and notably
// not uniformly: Google discounts input and output on Batch but not cache
// reads.
type Tier string

// The service tiers providers price separately. Only standard is seeded in
// v1; the rest exist so a rate card can arrive as rows rather than as a
// migration.
const (
	TierStandard Tier = "standard"
	TierFlex     Tier = "flex"
	TierPriority Tier = "priority"
	TierFast     Tier = "fast"
	TierBatch    Tier = "batch"
	TierGeoUS    Tier = "geo_us"
)

// AllTiers mirrors the database CHECK constraint.
var AllTiers = []Tier{
	TierStandard, TierFlex, TierPriority, TierFast, TierBatch, TierGeoUS,
}

// Valid reports whether t is a known service tier.
func (t Tier) Valid() bool {
	for _, known := range AllTiers {
		if t == known {
			return true
		}
	}
	return false
}

// Billable reports whether a kind is charged in its own right, or is a
// breakdown of one that already is.
//
// Reasoning is the second sort. Every provider that reports thinking tokens
// counts them inside its output total as well: OpenAI's completion_tokens
// includes reasoning_tokens, Google's normalisation sets output to
// candidatesTokenCount + thoughtsTokenCount, and Anthropic states that
// thinking is billed as output. The count is kept because an operator wants
// to see what a model spent thinking, but pricing it a second time would
// charge those tokens twice.
//
// Before this existed, a reasoning count with no rate behind it marked every
// thinking request partially_priced — a correct cost carrying a flag that
// said it might not be. The alternative fix, seeding a reasoning rate equal
// to output, would have made the flag go away by making the figure wrong.
func (k Kind) Billable() bool {
	switch k {
	case KindReasoning:
		return false
	default:
		return true
	}
}

// CountsTowardInputTier reports whether usage of this kind counts towards
// the input total that selects a context tier. OpenAI's threshold is
// measured on input tokens alone, and cached and cache-write tokens are
// part of the input the model receives.
func (k Kind) CountsTowardInputTier() bool {
	switch k {
	case KindInput, KindCachedRead, KindCacheWrite5m, KindCacheWrite1h, KindAudioInput:
		return true
	default:
		return false
	}
}
