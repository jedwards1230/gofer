package tui

// stats.go implements the /stats command-panel tab: SESSION LIFECYCLE PLUS
// PORTFOLIO-WIDE COUNTS. For the current session it renders age
// (Created→now), last-active (Updated→now), status, and model; beneath that a
// roster rollup — the number of sessions and the summed tokens + summed cost
// across every roster row. It is the counterpart to the Usage tab: /usage
// answers "where did THIS session's tokens and money go", /stats answers "how
// old is this session and how much has the whole fleet spent". Like
// [statusView] it is a pure, read-only value that OMITS a row it can't answer
// (an unset timestamp, no active session) rather than blank-filling it.
//
// deferred (#175): the per-turn activity roll-up line ("read N files, ran M
// commands") the issue flags as M8 polish is out of scope — it needs
// per-tool-call tallying off the event stream this roster-snapshot projection
// does not consume.

import (
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// statsView renders the Stats tab. sess is nil on the overview (the session
// lifecycle rows are then omitted, leaving just the roster rollup); now is the
// overview's reference time so the elapsed rows stay deterministic in goldens;
// roster is the full session snapshot the rollup sums across.
type statsView struct {
	theme  theme.Theme
	sess   *SessionInfo // nil on the overview — no active session lifecycle to show
	now    time.Time
	roster []SessionInfo
}

// View renders the view's rows, one per line, width-truncated and capped to
// height — the same Renderable contract every other panel component follows
// ([testkit.Renderable]).
func (v statsView) View(width, height int) string {
	lines := v.lines()
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the session-lifecycle rows (when a session is active) followed
// by the roster rollup, omitting any row the current data can't answer.
func (v statsView) lines() []string {
	var out []string
	out = append(out, v.sessionLines()...)
	out = append(out, v.rosterLines()...)
	return out
}

// sessionLines renders the current session's lifecycle: age, last-active,
// status, model. Empty on the overview (no active session). Each elapsed row
// is omitted when its timestamp is unset rather than aging against the zero
// time, which would render a nonsensical multi-decade duration.
func (v statsView) sessionLines() []string {
	if v.sess == nil {
		return nil
	}
	out := []string{"Session: " + orDash(v.sess.Title)}
	if !v.sess.Created.IsZero() {
		out = append(out, "Age: "+humanAge(v.now.Sub(v.sess.Created)))
	}
	if !v.sess.Updated.IsZero() {
		out = append(out, "Last active: "+humanDuration(v.now.Sub(v.sess.Updated)))
	}
	out = append(out, "Status: "+v.sess.Status.String())
	out = append(out, "Model: "+orDash(v.sess.Model))
	return out
}

// rosterLines renders the portfolio rollup: how many sessions the roster holds
// and the summed tokens + cost across all of them. Tokens sum every normalized
// bucket (input, output, cache read, cache write) — total throughput, not just
// billed input/output — so the count is the whole token volume the fleet moved.
func (v statsView) rosterLines() []string {
	tokens, cost := 0, 0.0
	for _, s := range v.roster {
		u := s.Usage
		tokens += u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
		cost += s.Cost.USD
	}
	out := []string{"Sessions: " + strconv.Itoa(len(v.roster))}
	out = append(out, "Total tokens: "+strconv.Itoa(tokens))
	if cost == 0 {
		out = append(out, "Total cost: —")
	} else {
		out = append(out, "Total cost: "+fmtUSD(cost))
	}
	return out
}
