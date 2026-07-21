package tui

import (
	"sort"
	"time"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// Overview is the roster screen: a header with status counts, a scrollable
// list of sessions in one of two orderings, and a persistent dispatch bar for
// starting a new session by typing. It is a pure value like [Model]: most
// methods return an updated copy, so a fixed sequence of inputs replays to the
// same rendered output in every golden test. The exception is
// [Overview.TakeSubmitted], which has a pointer receiver and mutates in place
// to ensure its take-once semantics (each dispatched prompt is observed
// exactly once).
type Overview struct {
	theme theme.Theme
	meta  OverviewMeta

	sessions []SessionInfo
	view     rosterView

	// selectedID tracks selection by session id, not row index, so toggling
	// the view (which reorders rows) keeps the same session selected.
	selectedID string

	// input is the dispatch-bar buffer — a cursor-aware [inputBuffer]
	// (inputbuf.go), not just an append-only string; submitted/hasSubmitted
	// mirror [Model]'s take-once submission handoff.
	input        inputBuffer
	submitted    string
	hasSubmitted bool
}

// OverviewMeta is the static header context: the app identity plus the current
// model and working directory, and the reference time the roster ages rows
// against (fixed in tests, time.Now in the live adapter).
type OverviewMeta struct {
	App     string
	Version string
	Model   string
	Cwd     string
	Now     time.Time

	// AttachSessionID, when set, opens the app directly on that session's
	// attach screen instead of the overview — the `gofer attach <id>` entry
	// point. Empty (the default) starts on the overview.
	AttachSessionID string
}

// rosterView selects the roster ordering.
type rosterView int

const (
	// viewFlat lists every session most-recently-active first, grouped under a
	// cwd header per working directory.
	viewFlat rosterView = iota
	// viewGrouped splits the list into Working / Needs input / Finished
	// sections, each most-recently-active first.
	viewGrouped
)

// NewOverview returns an empty roster screen rendering through th.
func NewOverview(th theme.Theme, meta OverviewMeta) Overview {
	return Overview{theme: th, meta: meta}
}

// WithSessions returns a copy of the overview showing sessions. Selection is
// preserved by id when the previously selected session is still present,
// otherwise it falls to the first row of the current ordering.
func (o Overview) WithSessions(sessions []SessionInfo) Overview {
	o.sessions = append([]SessionInfo(nil), sessions...)
	o.selectedID = o.resolveSelection(o.selectedID)
	return o
}

// ToggleView flips between the flat and grouped orderings, keeping the
// selected session selected across the reorder.
func (o Overview) ToggleView() Overview {
	if o.view == viewFlat {
		o.view = viewGrouped
	} else {
		o.view = viewFlat
	}
	return o
}

// MoveDown moves selection to the next row in the current ordering.
func (o Overview) MoveDown() Overview { return o.move(1) }

// MoveUp moves selection to the previous row in the current ordering.
func (o Overview) MoveUp() Overview { return o.move(-1) }

// move shifts selection by delta rows within the current ordering, clamping at
// the ends.
func (o Overview) move(delta int) Overview {
	rows := o.ordered()
	if len(rows) == 0 {
		return o
	}
	cur := 0
	for i, s := range rows {
		if s.ID == o.selectedID {
			cur = i
			break
		}
	}
	next := cur + delta
	if next < 0 {
		next = 0
	}
	if next >= len(rows) {
		next = len(rows) - 1
	}
	o.selectedID = rows[next].ID
	return o
}

// SelectedID returns the id of the currently selected session, or "" when the
// roster is empty.
func (o Overview) SelectedID() string { return o.selectedID }

// DefaultModel returns the resolved credential-driven default model this
// overview's header shows (meta.Model) — "" if none/ambiguous. The command
// panel's /status view falls back to this when no session is active, or the
// active session carries no model override.
func (o Overview) DefaultModel() string { return o.meta.Model }

// Now returns the reference time the overview ages rows against (meta.Now) —
// fixed in golden tests, time.Now() in the live adapter. The command panel
// captures it at open time so the Stats tab's elapsed output (age, last-active)
// stays deterministic across renders (see stats.go).
func (o Overview) Now() time.Time { return o.meta.Now }

// Roster returns a copy of the overview's current session snapshot, for the
// command panel's Stats rollup (session count + summed tokens/cost across all
// rows). A copy so the panel's captured slice can't drift under a later
// WithSessions poll.
func (o Overview) Roster() []SessionInfo { return append([]SessionInfo(nil), o.sessions...) }

// WithDefaultModel returns a copy of the overview whose header reports model
// as the default (meta.Model). It is how `/model` makes its effect visible
// without a restart (issue #156): meta was seeded once at NewApp time from a
// value cmd/gofer resolved at startup, so a later change to the default left
// the header — and the attach screen's header, which renders the same meta
// (see attachHeaderLines) — asserting a model no new session would use.
//
// It is deliberately NOT called on every `/model`: the caller decides, because
// the header is only this process's to update when this process owns the
// default. Attached to a daemon it shows the DAEMON's default (off
// gofer/hello), which a local config write does not change — see
// [App.handleModelSelect].
func (o Overview) WithDefaultModel(model string) Overview {
	o.meta.Model = model
	return o
}

// Selected returns the currently selected session's full info and true, or a
// zero value and false when the roster is empty. The app root reads it to
// route ctrl-x to kill (running) or archive (finished).
func (o Overview) Selected() (SessionInfo, bool) {
	for _, s := range o.sessions {
		if s.ID == o.selectedID {
			return s, true
		}
	}
	return SessionInfo{}, false
}

// TypeRune inserts r into the dispatch-bar buffer at the cursor.
func (o Overview) TypeRune(r rune) Overview {
	o.input = o.input.InsertRune(r)
	return o
}

// InsertText inserts s into the dispatch-bar buffer at the cursor —
// key.Text can in principle carry more than one rune (an IME commit).
func (o Overview) InsertText(s string) Overview {
	o.input = o.input.InsertText(s)
	return o
}

// Backspace removes the rune immediately before the cursor, if any.
func (o Overview) Backspace() Overview {
	o.input = o.input.Backspace()
	return o
}

// DeleteForward removes the rune at the cursor, if any — Delete/Ctrl+D.
func (o Overview) DeleteForward() Overview {
	o.input = o.input.DeleteForward()
	return o
}

// MoveLeft/MoveRight move the dispatch-bar cursor one rune.
func (o Overview) MoveLeft() Overview  { o.input = o.input.MoveLeft(); return o }
func (o Overview) MoveRight() Overview { o.input = o.input.MoveRight(); return o }

// MoveWordLeft/MoveWordRight move the dispatch-bar cursor one word —
// Alt+Left/Alt+Right.
func (o Overview) MoveWordLeft() Overview  { o.input = o.input.MoveWordLeft(); return o }
func (o Overview) MoveWordRight() Overview { o.input = o.input.MoveWordRight(); return o }

// MoveHome/MoveEnd jump the dispatch-bar cursor to the buffer's start/end —
// Home/Ctrl+A and End/Ctrl+E.
func (o Overview) MoveHome() Overview { o.input = o.input.MoveHome(); return o }
func (o Overview) MoveEnd() Overview  { o.input = o.input.MoveEnd(); return o }

// DeleteWordBackward deletes the word before the cursor — Alt+Backspace/Ctrl+W.
func (o Overview) DeleteWordBackward() Overview {
	o.input = o.input.DeleteWordBackward()
	return o
}

// DeleteToLineStart/DeleteToLineEnd delete from the cursor to the buffer's
// start/end — Ctrl+U and Ctrl+K.
func (o Overview) DeleteToLineStart() Overview {
	o.input = o.input.DeleteToLineStart()
	return o
}

func (o Overview) DeleteToLineEnd() Overview {
	o.input = o.input.DeleteToLineEnd()
	return o
}

// InputEmpty reports whether the dispatch bar has no pending text. The app
// root consults this to resolve the navigation contract's left-arrow (← in an
// empty input backs out; with text it edits).
func (o Overview) InputEmpty() bool { return o.input.Empty() }

// SetInput replaces the dispatch-bar buffer outright, cursor moving to the
// end — used by the command menu's Enter-select (command_menu.go), which
// clears the buffer wholesale rather than one rune at a time.
func (o Overview) SetInput(s string) Overview {
	o.input = o.input.SetText(s)
	return o
}

// SetInputCursor replaces the dispatch-bar buffer and places the cursor
// explicitly — used by the command menu's Tab-complete (command_menu.go),
// which splices a completion in place of the active token and wants the
// cursor right after it, not at the end of any trailing text the splice
// left in place.
func (o Overview) SetInputCursor(s string, cursor int) Overview {
	o.input = o.input.SetTextCursor(s, cursor)
	return o
}

// Submit records the dispatch-bar buffer as submitted (retrievable via
// [Overview.TakeSubmitted]) and clears it. Submitting an empty buffer is a
// no-op.
func (o Overview) Submit() Overview {
	if o.input.Empty() {
		return o
	}
	o.submitted = o.input.String()
	o.hasSubmitted = true
	o.input = inputBuffer{}
	return o
}

// TakeSubmitted returns the text from the most recent [Overview.Submit] and
// clears it, so each dispatched prompt is observed exactly once. The app root
// forwards it to [Supervisor.Create] and attaches into the new session.
func (o *Overview) TakeSubmitted() (string, bool) {
	if !o.hasSubmitted {
		return "", false
	}
	text := o.submitted
	o.submitted = ""
	o.hasSubmitted = false
	return text, true
}

// resolveSelection returns want if a session with that id is still present,
// otherwise the first row of the current ordering (or "" when empty).
func (o Overview) resolveSelection(want string) string {
	rows := o.ordered()
	for _, s := range rows {
		if s.ID == want {
			return want
		}
	}
	if len(rows) > 0 {
		return rows[0].ID
	}
	return ""
}

// ordered returns the sessions in the current view's row order: recency-first
// within the whole list (flat) or within each status group (grouped).
func (o Overview) ordered() []SessionInfo {
	if o.view == viewGrouped {
		var rows []SessionInfo
		for _, st := range []SessionStatus{StatusWorking, StatusNeedsInput, StatusFinished} {
			rows = append(rows, byRecency(o.filter(st))...)
		}
		return rows
	}
	return byRecency(append([]SessionInfo(nil), o.sessions...))
}

// filter returns the sessions in effective status st.
func (o Overview) filter(st SessionStatus) []SessionInfo {
	var out []SessionInfo
	for _, s := range o.sessions {
		if effectiveStatus(s) == st {
			out = append(out, s)
		}
	}
	return out
}

// byRecency sorts sessions most-recently-active first, breaking ties by id for
// a stable order.
func byRecency(s []SessionInfo) []SessionInfo {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Updated.Equal(s[j].Updated) {
			return s[i].ID < s[j].ID
		}
		return s[i].Updated.After(s[j].Updated)
	})
	return s
}

// effectiveStatus is the roster bucket a session actually belongs in. A
// pending permission request keeps the daemon's coarse Status at
// StatusWorking (the turn is technically in flight), but from the roster's
// point of view the session is blocked awaiting the user — the same
// condition the row's status-word color keys on. Finished always wins.
func effectiveStatus(s SessionInfo) SessionStatus {
	if s.Status == StatusFinished {
		return StatusFinished
	}
	if s.Pending > 0 {
		return StatusNeedsInput
	}
	return s.Status
}

// counts tallies the roster by effective status for the header line.
func (o Overview) counts() (working, needsInput, finished int) {
	for _, s := range o.sessions {
		switch effectiveStatus(s) {
		case StatusWorking:
			working++
		case StatusNeedsInput:
			needsInput++
		case StatusFinished:
			finished++
		}
	}
	return working, needsInput, finished
}
