package tui

import (
	"sort"
	"time"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// Overview is the roster screen: a header with status counts, a scrollable
// list of sessions in one of two orderings, and a persistent dispatch bar for
// starting a new session by typing. It is a pure value like [Model] — every
// method returns an updated copy, so a fixed sequence of inputs replays to the
// same rendered output in every golden test.
type Overview struct {
	theme theme.Theme
	meta  OverviewMeta

	sessions []SessionInfo
	view     rosterView

	// selectedID tracks selection by session id, not row index, so toggling
	// the view (which reorders rows) keeps the same session selected.
	selectedID string

	// input is the dispatch-bar buffer; submitted/hasSubmitted mirror
	// [Model]'s take-once submission handoff.
	input        string
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
}

// rosterView selects the roster ordering.
type rosterView int

const (
	// viewFlat lists every session in one list, most-recently-active first.
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

// TypeRune appends r to the dispatch-bar buffer.
func (o Overview) TypeRune(r rune) Overview {
	o.input += string(r)
	return o
}

// Backspace removes the last rune from the dispatch-bar buffer, if any.
func (o Overview) Backspace() Overview {
	if o.input == "" {
		return o
	}
	runes := []rune(o.input)
	o.input = string(runes[:len(runes)-1])
	return o
}

// InputEmpty reports whether the dispatch bar has no pending text. The app
// root consults this to resolve the navigation contract's left-arrow (← in an
// empty input backs out; with text it edits).
func (o Overview) InputEmpty() bool { return o.input == "" }

// Submit records the dispatch-bar buffer as submitted (retrievable via
// [Overview.TakeSubmitted]) and clears it. Submitting an empty buffer is a
// no-op.
func (o Overview) Submit() Overview {
	if o.input == "" {
		return o
	}
	o.submitted = o.input
	o.hasSubmitted = true
	o.input = ""
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

// filter returns the sessions in status st.
func (o Overview) filter(st SessionStatus) []SessionInfo {
	var out []SessionInfo
	for _, s := range o.sessions {
		if s.Status == st {
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

// counts tallies the roster by status for the header line.
func (o Overview) counts() (working, needsInput, finished int) {
	for _, s := range o.sessions {
		switch s.Status {
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
