package tui

// paste.go handles bracketed paste ([tea.PasteMsg]) — the terminal handing
// the app a whole clipboard payload in ONE message instead of replaying it as
// synthetic key presses. Bracketed paste is already on: bubbletea enables it
// by default (only the per-view DisableBracketedPasteMode opt-out, which
// gofer never sets, turns it off), so before this the message simply arrived
// and was ignored, and every paste was silently swallowed.
//
// The payload is inserted as a SINGLE insertion at the focused surface's
// cursor and never routed through the key handlers. That is the whole point:
// a key handler would read the paste's own characters as bindings — an
// embedded newline would SUBMIT mid-paste (turning one pasted prompt into
// several truncated ones), and a leading space in peek would close the screen.
// Insertion at the cursor is exactly what typing already does
// (applyInputKey's key.Text catch-all routes through the same InsertText), so
// paste is deliberately the typed-text path with the key interpretation
// removed, not a second insertion primitive.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
)

// handlePaste inserts a pasted payload into whichever text-entry surface is
// focused: the peek card's reply (a plain string with no cursor, so appended
// exactly as its typed path appends), the attach input, or the overview
// dispatch bar — routed on a.scr, the same way handleKey routes a key press.
//
// Paste follows typed text's precedence exactly: where a typed rune would be
// swallowed by an overlay that owns the keyboard, so is a paste. That is the
// command panel (its own filter/edit line has no cursor-aware buffer to
// insert into) and the attach screen's pending-approval prompt (a modal
// answer-y/n gate where typing does nothing either) — EXCEPT while that
// prompt's amend editor is open, which is a real text surface a typed rune
// edits, so a paste edits it too. Anything a typed rune would edit, a paste
// edits.
func (a App) handlePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	amending := a.scr == screenAttach && a.sess.AmendingApproval()
	if a.panel != nil || (a.scr == screenAttach && a.sess.HasPendingApproval() && !amending) {
		return a, nil
	}

	a.clearStatus()
	// A paste mutates the buffer under the selection just like a key press
	// does, so it clears an active/frozen mouse selection for the same reason
	// (docs/TUI.md's "clear the selection on the next click / a key press").
	a.sel = nil

	text, clipped := clipPaste(normalizeNewlines(msg.Content), a.pasteLimitBytes())
	if text == "" {
		return a, nil
	}

	switch {
	case amending:
		a.sess = a.sess.InsertApprovalAmendText(text)
	case a.scr == screenPeek:
		a.peekReply += text
	case a.scr == screenAttach:
		a.sess = a.sess.InsertText(text)
	default:
		a.over = a.over.InsertText(text)
	}

	if clipped {
		// A caveat, not a failure: the paste DID land, just truncated.
		a.setStatus(sevWarn, fmt.Sprintf("paste clipped to %d bytes (tui.max_paste_bytes)", a.pasteLimitBytes()))
	}
	// A pasted "/mod" (or "@internal/") is as much an active token as a typed
	// one, so the autocomplete menu re-syncs off a paste exactly as it does
	// after every per-screen key handler (see Update) — carrying syncMenu's
	// own follow-on command, which for an `@` token is the off-loop cwd
	// enumeration (filemention.go).
	return a.syncMenu()
}

// pasteLimitBytes reports the effective tui.max_paste_bytes cap
// (config.TUI.PasteLimitBytes — default config.DefaultMaxPasteBytes, 0 = no
// limit), read off a.commandEnv.Config() on every call rather than cached —
// the same "always current, never a stale snapshot" contract
// autoscrollEnabled/mouseEnabled follow. A nil Config closure or a read error
// both fall through to the default.
func (a App) pasteLimitBytes() int {
	if a.commandEnv.Config == nil {
		return config.DefaultMaxPasteBytes
	}
	cfg, err := a.commandEnv.Config()
	if err != nil {
		return config.DefaultMaxPasteBytes
	}
	return cfg.TUI.PasteLimitBytes()
}

// normalizeNewlines rewrites a paste's line endings to plain "\n". Bracketed
// paste carries the clipboard's bytes verbatim, and terminals commonly encode
// a pasted line break as CR (or CRLF from Windows-authored text) — a CR that
// reached a terminal write would return the cursor to column 0 and overprint
// the line. Only the ENCODING of a line break changes here; the break itself
// survives, so a multi-line paste is still multi-line when it is submitted.
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// clipPaste caps s at limit bytes, cut on a rune boundary so the buffer never
// holds half a UTF-8 sequence, and reports whether it clipped. limit <= 0
// means no cap. Callers surface a clip on the status line — the operator sees
// that content was dropped rather than silently submitting a truncated
// prompt.
func clipPaste(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// displaySafe replaces every C0 control character (and DEL) with its Unicode
// Control Pictures glyph — "\n" renders as "␊", tab as "␉" — for the
// single-line inputs a pasted payload can now put them in.
//
// This is a RENDER-only substitution: the buffer keeps the real control
// characters, so a multi-line paste is submitted with its newlines intact and
// reaches the agent as the operator pasted it. Rendering them literally is
// what actually breaks — the input line is one element of a fixed-height row
// slice that the frame joins with "\n", so a literal newline inside it emits
// extra rows, pushing the frame past its height budget and corrupting the
// layout (and a literal ESC/CR would drive the terminal directly). The
// substitution is one rune per rune, so it also leaves every cursor-splice
// and display-width measurement downstream unchanged.
func displaySafe(s string) string {
	if !strings.ContainsFunc(s, isC0Control) {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x7f:
			return '␡' // U+2421 SYMBOL FOR DELETE
		case isC0Control(r):
			return 0x2400 + r // U+2400 block: one picture per C0 control
		default:
			return r
		}
	}, s)
}

func isC0Control(r rune) bool { return r < 0x20 || r == 0x7f }
