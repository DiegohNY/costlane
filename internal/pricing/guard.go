package pricing

// TierGuardFactor is the default fraction of a context threshold at which a
// reservation starts pricing at the higher tier.
//
// The estimate that feeds a reservation is chars/4, which understates token
// counts by 20-30% on code, JSON and non-English text — always in the same
// direction, more tokens than predicted. Crossing OpenAI's threshold doubles
// the price of the whole request, so an estimate landing just under it would
// under-reserve by 100%.
//
// Reserving high costs almost nothing: the reservation is released at settle,
// so the only visible effect is a refusal on a borderline request from a key
// whose remaining budget sits between one and two times that request. That
// refusal is correct behaviour for a product promising not to overspend.
//
// Running an exact tokenizer here would not help. It does not remove the
// ambiguity — the provider's own count still differs by a few percent for
// tools, system prompts and images — and it adds latency to precisely the
// heaviest requests.
const TierGuardFactor = 0.75

// GuardedTierTotal returns the input total to use when selecting a tier for a
// reservation. An estimate at or above factor × threshold is treated as being
// over that threshold.
//
// The band is deliberately asymmetric: it only ever pushes an estimate up,
// because the estimator only ever errs downward.
func GuardedTierTotal(estimate int64, thresholds []int64, factor float64) int64 {
	guarded := estimate
	for _, threshold := range thresholds {
		if threshold <= 0 || estimate >= threshold {
			continue
		}
		if float64(estimate) >= factor*float64(threshold) {
			guarded = max(guarded, threshold)
		}
	}
	return guarded
}

// Thresholds lists the context-tier boundaries this table defines for a
// model, so a reservation knows which cliffs it might be near.
func (t *Table) Thresholds(model, provider string, tier Tier) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, r := range t.rows[modelKey{model, provider}] {
		if r.Tier == tier && r.TierFrom > 0 && !seen[r.TierFrom] {
			seen[r.TierFrom] = true
			out = append(out, r.TierFrom)
		}
	}
	return out
}
