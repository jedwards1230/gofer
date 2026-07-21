package tui

// usage.go implements the /usage command-panel tab: WHERE THE TOKENS AND MONEY
// WENT for the current session. It renders the accumulated
// [SessionInfo.Usage] (input / output / cache-read / cache-write tokens) and
// the [SessionInfo.Cost] breakdown (USD total plus the per-bucket USD when
// non-zero) that already flow off the daemon's session/update. Like
// [statusView] it is a pure, read-only value that OMITS any row the current
// data can't answer rather than blank-filling it — and when no turn has
// completed (usage all-zero) it renders one honest "no usage recorded yet"
// line instead of a wall of zeros.
//
// deferred (#175): true per-message / per-tool-call token attribution is out
// of scope — it needs SDK per-item usage granularity absent from v0.14.2,
// which reports usage only at the turn and session level (the accumulated
// [provider.Usage] this view reads). Rendering a synthesized per-message
// estimate as fact is exactly what the issue forbids, so this view shows only
// the session-level accumulation the contract carries today.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// usageView renders the Usage tab: the current session's accumulated token
// and cost consumption. sess is nil on the overview (no active session) and
// when its usage is all-zero (no turn finished yet); both collapse to a single
// honest line rather than a table of dashes or zeros.
type usageView struct {
	theme theme.Theme
	sess  *SessionInfo // nil on the overview — no active session
}

// View renders the view's rows, one per line, width-truncated and capped to
// height — the same Renderable contract every other panel component follows
// ([testkit.Renderable]).
func (v usageView) View(width, height int) string {
	lines := v.lines()
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the token/cost rows in table order. With no active session, or
// with a session that has recorded no usage yet, it returns a single muted
// "no usage recorded yet" line — the house-style honest empty state, never a
// wall of zeros.
func (v usageView) lines() []string {
	if v.sess == nil {
		return []string{v.theme.MutedStyle().Render("No active session — attach to see its usage.")}
	}
	u := v.sess.Usage
	if usageIsZero(u) {
		return []string{v.theme.MutedStyle().Render("No usage recorded yet.")}
	}

	out := []string{
		"Input tokens: " + strconv.Itoa(u.InputTokens),
		"Output tokens: " + strconv.Itoa(u.OutputTokens),
	}
	// Cache rows are omitted rather than shown as zero: a provider that does no
	// caching (or a turn that read/wrote none) has nothing to report here.
	if u.CacheReadTokens > 0 {
		out = append(out, "Cache read tokens: "+strconv.Itoa(u.CacheReadTokens))
	}
	if u.CacheWriteTokens > 0 {
		out = append(out, "Cache write tokens: "+strconv.Itoa(u.CacheWriteTokens))
	}
	out = append(out, v.costLines(v.sess.Cost)...)
	return out
}

// costLines renders the Cost total plus its per-bucket breakdown. The total is
// shown as a dash when zero — a session priced against an unregistered model
// (unknown pricing) reports $0, and claiming "$0.0000" there would present
// unpriced usage as free. Each breakdown row is omitted when its bucket is
// zero, matching the token rows' omit-don't-blank-fill discipline.
func (v usageView) costLines(c provider.Cost) []string {
	if c.USD == 0 {
		return []string{"Cost: —"}
	}
	out := []string{"Cost: " + fmtUSD(c.USD)}
	if c.InputUSD > 0 {
		out = append(out, "  Input: "+fmtUSD(c.InputUSD))
	}
	if c.OutputUSD > 0 {
		out = append(out, "  Output: "+fmtUSD(c.OutputUSD))
	}
	if c.CacheReadUSD > 0 {
		out = append(out, "  Cache read: "+fmtUSD(c.CacheReadUSD))
	}
	if c.CacheWriteUSD > 0 {
		out = append(out, "  Cache write: "+fmtUSD(c.CacheWriteUSD))
	}
	return out
}

// usageIsZero reports whether u carries no token counts at all — the "no turn
// has completed yet" state the Usage tab collapses to one honest line.
func usageIsZero(u provider.Usage) bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0
}

// fmtUSD formats a USD amount at the same four-decimal precision the attach
// footer's per-turn cost uses ([Model.statusLine]), so the two surfaces read
// consistently. Shared with the Stats tab's roster-cost rollup (stats.go).
func fmtUSD(usd float64) string {
	return fmt.Sprintf("$%.4f", usd)
}
