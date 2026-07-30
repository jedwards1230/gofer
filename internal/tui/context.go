package tui

// context.go implements the /context command-panel tab (gofer#177): CONTEXT-
// WINDOW PRESSURE, so it is legible before it bites, not just after
// compaction already fired. Modeled directly on the Usage/Stats tabs'
// omit-what-you-can't-answer shape (status.go, usage.go, stats.go).
//
// # Data model — reused, not invented
//
// gofer has no tokenizer (issue #177 flags this as a hard blocker for a
// per-category composition breakdown: system prompt / tool schemas / skills /
// messages, each its own count). Rather than build one, this view reads the
// SAME measured figures the automatic compaction trigger uses
// (internal/supervisor's shouldAutoCompact): [SessionInfo.LastUsage] — the
// most recently completed turn's InputTokens (+ CacheReadTokens), a REAL
// number the provider itself computed to answer that call, not an estimate —
// against [SessionInfo.ContextWindow], the active model's registered
// capacity. That is a single aggregate "how full is the window" fact, not
// gofer#177's full category grid; the category breakdown stays blocked on the
// tokenizer primitive that issue names, and is not attempted here.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// contextBarWidth is the fixed cell count of the /context fill bar — wide
// enough to show meaningful gradations (each cell is 5%), narrow enough to
// fit the panel's fixed body-row budget alongside its label.
const contextBarWidth = 20

// contextView renders the Context tab: the current session's context-window
// fill. sess is nil on the overview (no active session) and when it carries
// no settled turn yet, both of which collapse to a single honest line.
type contextView struct {
	theme theme.Theme
	sess  *SessionInfo // nil on the overview — no active session
	env   CommandEnv   // for reading the configured auto-compaction threshold
}

// View renders the view's rows, one per line, width-truncated and capped to
// height — the same Renderable contract every other panel component follows.
func (v contextView) View(width, height int) string {
	lines := v.lines()
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the context-pressure rows. With no active session, or one
// with no settled turn yet, it returns a single muted line rather than a
// wall of zeros or a divide-by-nothing percentage.
func (v contextView) lines() []string {
	if v.sess == nil {
		return []string{v.theme.MutedStyle().Render("No active session — attach to see its context usage.")}
	}
	used := v.sess.LastUsage.InputTokens + v.sess.LastUsage.CacheReadTokens
	if used == 0 {
		return []string{v.theme.MutedStyle().Render("No turn has completed yet — nothing measured.")}
	}

	out := []string{"Model: " + orDash(v.sess.Model)}
	if v.sess.ContextWindow <= 0 {
		// Unknown window (an unregistered model): counts only, per house rule
		// — 0 means unknown, never "no window", so no percentage is shown and
		// nothing is divided by it.
		out = append(out, "Context window: unknown for this model")
		out = append(out, "Last measured input: "+strconv.Itoa(used)+" tokens")
		return out
	}

	pct := float64(used) / float64(v.sess.ContextWindow) * 100
	free := v.sess.ContextWindow - used
	if free < 0 {
		free = 0
	}
	out = append(out,
		"Context window: "+strconv.Itoa(v.sess.ContextWindow)+" tokens",
		contextBar(used, v.sess.ContextWindow)+fmt.Sprintf(" %.0f%%", pct),
		"In use: "+strconv.Itoa(used)+" tokens (measured — this session's last turn)",
		"Free: "+strconv.Itoa(free)+" tokens",
	)
	out = append(out, v.thresholdLine())
	return out
}

// thresholdLine reports the configured automatic-compaction threshold, read
// through the same CommandEnv.Config seam /config uses — honest about being
// a POLICY value, not a measurement, unlike every row above it. A read
// failure or a nil Config closure (the overview's zero-value CommandEnv)
// omits the row rather than guessing gofer's built-in default, which could
// silently disagree with an operator's actual config.json.
func (v contextView) thresholdLine() string {
	if v.env.Config == nil {
		return ""
	}
	cfg, err := v.env.Config()
	if err != nil {
		return ""
	}
	if !cfg.Compaction.AutoEnabled() {
		return "Auto-compaction: off (/compact still available)"
	}
	return fmt.Sprintf("Auto-compacts at: %.0f%% (/compact to do it now)", cfg.Compaction.Threshold()*100)
}

// contextBar renders a fixed-width block-cell fill bar: ⌈used/window⌉ of
// contextBarWidth cells filled, the rest empty — the single-aggregate visual
// this view can honestly draw without a per-category tokenizer (see the
// package doc). Clamped to the bar's width so usage at or past 100% (the
// window the provider was actually called against can exceed the registry's
// static capacity, e.g. a temporarily larger context beta) still renders a
// full, not overflowing, bar.
func contextBar(used, window int) string {
	filled := 0
	if window > 0 {
		filled = used * contextBarWidth / window
	}
	if filled > contextBarWidth {
		filled = contextBarWidth
	}
	if filled < 0 {
		filled = 0
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", contextBarWidth-filled) + "]"
}
