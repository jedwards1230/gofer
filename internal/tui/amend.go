package tui

// amend.go implements the approval prompt's inline amend editor: Tab on a
// pending permission request opens [amendEditor] prefilled with the gated
// call's command body, the user edits it, and ctrl+s approves the EDITED
// call (see dialog.go for the key routing, approval.go for the render).
//
// It is a multi-line editor built over [inputBuffer] — one buffer per line
// plus a cursor line — precisely so the within-line keymap is the app's
// existing one ([applyInputKey], input_keymap.go) rather than a second
// divergent copy of it. Only the keys that are inherently multi-line (the
// line-crossing arrows, ↑/↓, Enter, and the backspace that joins two lines)
// are handled here.
//
// Like every other TUI value it is copy-on-write: each method returns an
// updated copy, and [Model]'s mutators reallocate the pendingApproval they
// hang it off rather than writing through the pointer.

import (
	"encoding/json"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// amendEditor is the open editor for one amend: the edited text as one
// [inputBuffer] per line, which line the cursor is on, and the spec key the
// text will be written back to on commit (the same key [commandBody] picked
// for display).
//
// The zero value is not usable — lines is never empty for a live editor
// (newAmendEditor seeds at least one line, and every operation preserves
// that), so cursorLine always indexes a real buffer.
type amendEditor struct {
	lines      []inputBuffer
	cursorLine int
	key        string
}

// newAmendEditor opens an editor over body, split on its newlines, with the
// cursor at the end of the last line — the "you are about to change the tail
// of this command" position a prefilled editor should open at. An empty body
// still yields one (empty) line — strings.Split never returns an empty slice
// — so the editor is never line-less.
func newAmendEditor(key, body string) amendEditor {
	physical := strings.Split(body, "\n")
	lines := make([]inputBuffer, len(physical))
	for i, l := range physical {
		lines[i] = inputBuffer{}.SetText(l)
	}
	return amendEditor{lines: lines, cursorLine: len(lines) - 1, key: key}
}

// Text returns the editor's content as one string, lines rejoined with "\n" —
// what the amended tool input carries under the edited key.
func (e amendEditor) Text() string {
	parts := make([]string, len(e.lines))
	for i, l := range e.lines {
		parts[i] = l.String()
	}
	return strings.Join(parts, "\n")
}

// clone returns a copy of e with its own lines backing array, so a mutation
// below can never write through to the editor a caller still holds (the
// copy-on-write discipline [Model] follows for its own state).
func (e amendEditor) clone() amendEditor {
	lines := make([]inputBuffer, len(e.lines))
	copy(lines, e.lines)
	e.lines = lines
	return e
}

// cur returns the buffer the cursor is on.
func (e amendEditor) cur() inputBuffer { return e.lines[e.cursorLine] }

// withCur returns e with buf replacing the cursor line's buffer.
func (e amendEditor) withCur(buf inputBuffer) amendEditor {
	next := e.clone()
	next.lines[next.cursorLine] = buf
	return next
}

// applyKey applies one key press to the editor, returning the updated copy.
// Every key routes here while the editor is open — dialog.go peels off only
// ctrl+c (quit), ctrl+s (commit), and esc (cancel) first — so a/d/r/1/2 type
// characters instead of resolving the request underneath.
//
// The multi-line keys are handled first and the rest delegate to
// [applyInputKey], the same keymap the overview dispatch bar and the attach
// input use: word motion, ctrl+a/ctrl+e, ctrl+w/ctrl+u/ctrl+k, delete, and
// printable insertion all behave here exactly as they do there, for free. A
// key neither layer claims (Tab, an unbound function key) leaves the editor
// untouched.
func (e amendEditor) applyKey(key tea.Key) amendEditor {
	switch {
	case key.Code == tea.KeyEnter:
		return e.splitLine()
	case key.Code == tea.KeyUp:
		return e.moveLine(-1)
	case key.Code == tea.KeyDown:
		return e.moveLine(1)
	case key.Code == tea.KeyLeft && !key.Mod.Contains(tea.ModAlt) && e.cur().Cursor() == 0 && e.cursorLine > 0:
		// At a line's start, ← wraps to the END of the line above rather
		// than doing nothing — the boundary behavior every multi-line editor
		// has, and the only way to reach the previous line's tail without
		// ↑ + End.
		next := e.clone()
		next.cursorLine--
		next.lines[next.cursorLine] = next.lines[next.cursorLine].MoveEnd()
		return next
	case key.Code == tea.KeyRight && !key.Mod.Contains(tea.ModAlt) && e.atLineEnd() && e.cursorLine < len(e.lines)-1:
		// The mirror: → at a line's end wraps to the START of the line below.
		next := e.clone()
		next.cursorLine++
		next.lines[next.cursorLine] = next.lines[next.cursorLine].MoveHome()
		return next
	case key.Code == tea.KeyBackspace && !key.Mod.Contains(tea.ModAlt) && e.cur().Cursor() == 0 && e.cursorLine > 0:
		return e.joinPrev()
	}
	buf, _ := applyInputKey(e.cur(), key)
	return e.withCur(buf)
}

// insertText inserts s at the cursor, honoring its line breaks: the text
// between two "\n"s lands on its own line, exactly as typing it would. It is
// the paste path (paste.go) — a pasted correction is the likeliest way a
// real command gets into this editor, and a paste that did nothing while a
// typed rune edits would break paste.go's "anything a typed rune would edit,
// a paste edits" rule.
func (e amendEditor) insertText(s string) amendEditor {
	for i, part := range strings.Split(s, "\n") {
		if i > 0 {
			e = e.splitLine()
		}
		e = e.withCur(e.cur().InsertText(part))
	}
	return e
}

// atLineEnd reports whether the cursor sits past the last rune of its line.
func (e amendEditor) atLineEnd() bool {
	buf := e.cur()
	return buf.Cursor() >= len([]rune(buf.String()))
}

// splitLine breaks the cursor line in two at the cursor (Enter), leaving the
// cursor at the start of the new lower half.
func (e amendEditor) splitLine() amendEditor {
	buf := e.cur()
	r := []rune(buf.String())
	cur := clampCursor(buf.Cursor(), len(r))

	next := e.clone()
	next.lines[next.cursorLine] = inputBuffer{}.SetText(string(r[:cur]))
	next.lines = slices.Insert(next.lines, next.cursorLine+1, inputBuffer{}.SetTextCursor(string(r[cur:]), 0))
	next.cursorLine++
	return next
}

// joinPrev appends the cursor line onto the one above and removes it
// (backspace at column 0) — the inverse of [amendEditor.splitLine], without
// which a line break Enter inserted could never be taken back. The cursor
// lands at the join point, where the deleted break was.
func (e amendEditor) joinPrev() amendEditor {
	next := e.clone()
	prev := next.lines[next.cursorLine-1]
	joinAt := len([]rune(prev.String()))
	next.lines[next.cursorLine-1] = inputBuffer{}.SetTextCursor(prev.String()+next.cur().String(), joinAt)
	next.lines = slices.Delete(next.lines, next.cursorLine, next.cursorLine+1)
	next.cursorLine--
	return next
}

// moveLine moves the cursor delta lines (↑/↓), clamping to the editor's
// bounds and carrying the column over — clamped to the target line's length,
// so ↓ from a long line onto a short one lands at that line's end rather
// than off it. A move that would leave the editor is a no-op, not a wrap.
func (e amendEditor) moveLine(delta int) amendEditor {
	target := e.cursorLine + delta
	if target < 0 || target >= len(e.lines) {
		return e
	}
	col := e.cur().Cursor()
	next := e.clone()
	next.cursorLine = target
	line := next.lines[target]
	next.lines[target] = line.SetTextCursor(line.String(), col)
	return next
}

// visibleLines returns the window of line indexes [start, end) the render
// shows for a limit-row budget, scrolled the minimum distance needed to keep
// the cursor line inside it. Truncating the cursor away instead — the way the
// prompt body's "… +N more lines" collapse works — would leave the user
// typing somewhere they cannot see, so this scrolls rather than clips. A
// non-positive limit means uncapped (every line), matching
// [approvalBodyLines]' reading of the same config value.
func (e amendEditor) visibleLines(limit int) (start, end int) {
	if limit <= 0 || len(e.lines) <= limit {
		return 0, len(e.lines)
	}
	start = e.cursorLine - limit + 1
	if start < 0 {
		start = 0
	}
	if last := len(e.lines) - limit; start > last {
		start = last
	}
	return start, start + limit
}

// AmendingApproval reports whether the pending request's inline amend editor
// is open — the app root consults it to route every key to the editor
// instead of the a/d/r decision keys (see App.handleApprovalKey).
func (m Model) AmendingApproval() bool {
	return m.pending != nil && m.pending.amend != nil
}

// BeginApprovalAmend opens the amend editor on the pending request,
// prefilled with the gated call's command body. ok is false — and the Model
// is returned untouched — when nothing is pending, or when the spec carries
// no editable command key ([commandBody] found none): there is no honest
// thing to put in an editor for a call whose input is a structured payload,
// and an empty editor whose commit would blank the call is worse than no
// editor at all. The caller reports that as a status note (dialog.go).
func (m Model) BeginApprovalAmend() (Model, bool) {
	if m.pending == nil {
		return m, false
	}
	body, key := commandBody(m.pending.spec)
	if key == "" {
		return m, false
	}
	p := *m.pending
	ed := newAmendEditor(key, body)
	p.amend = &ed
	m.pending = &p
	return m, true
}

// CancelApprovalAmend closes the editor and discards the edit, leaving the
// request itself pending and its spec untouched (esc). A no-op when no
// editor is open.
func (m Model) CancelApprovalAmend() Model {
	if !m.AmendingApproval() {
		return m
	}
	p := *m.pending
	p.amend = nil
	m.pending = &p
	return m
}

// ApplyApprovalAmendKey applies one key press to the open editor. A no-op
// when no editor is open, so a stray key can never construct one.
func (m Model) ApplyApprovalAmendKey(key tea.Key) Model {
	if !m.AmendingApproval() {
		return m
	}
	p := *m.pending
	ed := p.amend.applyKey(key)
	p.amend = &ed
	m.pending = &p
	return m
}

// InsertApprovalAmendText inserts pasted text into the open editor, line
// breaks included. A no-op when no editor is open.
func (m Model) InsertApprovalAmendText(s string) Model {
	if !m.AmendingApproval() {
		return m
	}
	p := *m.pending
	ed := p.amend.insertText(s)
	p.amend = &ed
	m.pending = &p
	return m
}

// AmendedInput builds the replacement tool input the open editor commits to:
// the pending call's FULL original spec with the edited key's value replaced
// by the editor's text, re-marshalled whole.
//
// Marshalling the whole spec is the load-bearing part. The SDK substitutes
// [event.PermissionReply.Input] into the call wholesale (loop.awaitApproval
// assigns it to call.Input), so a reply carrying only the edited command
// would ERASE every other argument the model passed — a timeout, a working
// directory, a structured option. ok is false when no editor is open; an
// error means the spec did not survive re-marshalling (a value the decoded
// event carried that json cannot encode), in which case no reply is sent.
func (m Model) AmendedInput() (json.RawMessage, bool, error) {
	if !m.AmendingApproval() {
		return nil, false, nil
	}
	ed := m.pending.amend
	amended := make(map[string]any, len(m.pending.spec)+1)
	for k, v := range m.pending.spec {
		amended[k] = v
	}
	amended[ed.key] = ed.Text()
	raw, err := json.Marshal(amended)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}
