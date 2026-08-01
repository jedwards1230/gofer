package config

// DefaultCompactionThreshold is [Compaction.ThresholdFraction]'s default:
// 0.85 — compact once a turn's measured input-token usage (the closest
// available proxy for "how big is the folded context right now"; see
// [Compaction.Threshold]'s doc) reaches 85% of the active model's context
// window. 15% headroom is enough for the next turn's reply plus whatever a
// tool call appends, without compacting so eagerly that a normal session
// pays a summarization round trip it didn't need. Never hardcoded at the
// call site — see internal/supervisor's pump, the only reader.
const DefaultCompactionThreshold = 0.85

// Compaction configures gofer's automatic context-compaction trigger — WHEN
// a session compacts its history, not the summarization strategy itself
// (the SDK's [runner.Runner.Compact] / its Summarizer seam own that; gofer
// carries no opinion on prompt or model choice for the summary). The zero
// value is fully valid: automatic compaction is ON, and
// [Compaction.Threshold] resolves to [DefaultCompactionThreshold].
type Compaction struct {
	// ThresholdFraction is the fraction of a model's context window at which
	// gofer fires an automatic compaction, read through [Compaction.Threshold].
	// nil (unset), or a value outside (0, 1) exclusive, resolves to
	// [DefaultCompactionThreshold] — a bad or missing value fails toward the
	// safe built-in rather than toward "never compact" (0) or "always compact"
	// (a value >= 1, which would fire on the very first turn).
	ThresholdFraction *float64 `json:"threshold_fraction,omitempty"`

	// Disabled turns the THRESHOLD trigger off entirely; the explicit
	// `/compact` command stays available either way. Default false — automatic
	// compaction runs unless an operator opts out.
	//
	// It does NOT disable the failure-triggered recovery gofer performs when a
	// provider rejects a turn for exceeding the context window (see
	// internal/supervisor's recoverFromContextOverflow). That path is not a
	// policy about when to compact ahead of time — it is the only way out of an
	// already-wedged session, and gating it here would mean opting out of
	// proactive compaction also opted out of ever recovering from the overflow
	// it makes more likely.
	Disabled bool `json:"disabled,omitempty"`
}

// Threshold resolves [Compaction.ThresholdFraction]'s effective value:
// [DefaultCompactionThreshold] when unset or outside (0, 1) exclusive, else
// the configured fraction.
//
// The pressure ratio this is compared against is a turn's measured
// InputTokens (+ CacheReadTokens) over the model's ContextWindow — the same
// real, provider-tokenized number [runner.Runner.LastUsage] and a live
// turn.finished event carry, not an estimate from a local tokenizer (gofer
// has none). It is measured for THAT turn's own call, so unlike the
// compaction summarizer's own usage (see event.SessionCompacted's doc) it
// carries no instructions-prompt overhead to caveat.
func (c Compaction) Threshold() float64 {
	if c.ThresholdFraction == nil || *c.ThresholdFraction <= 0 || *c.ThresholdFraction >= 1 {
		return DefaultCompactionThreshold
	}
	return *c.ThresholdFraction
}

// AutoEnabled reports whether the automatic compaction trigger is on: the
// negation of Disabled, spelled as an accessor so a reader never has to
// remember the field's inverted polarity.
func (c Compaction) AutoEnabled() bool {
	return !c.Disabled
}
