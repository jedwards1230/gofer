package tui

// cwdprompt.go is the client half of jedwards1230/gofer#326: what the TUI does
// when a session cannot be attached because the directory it was RECORDED in no
// longer exists on disk.
//
// The daemon answers that one case with a typed signal rather than a bare
// invalid-params rejection (internal/daemon's resolveLoadCwd), and the bridge
// relays it to whichever client registered for it — see
// [daemonbridge.Supervisor.OnSessionCwdMissing], reached here through the
// builtin-only [cwdMissingNotifier] interface so this package needs no import of
// the daemon or the wire packages. It is the ONE attach failure a client can
// offer a remedy for instead of a message, so it renders as a three-way prompt:
//
//  1. re-init the session in a directory the user PICKS — sent as an explicit,
//     non-blank cwd, so a bad pick still hard-errors the ordinary way;
//  2. cancel — back to the overview, mutating nothing at all; and
//  3. archive / delete — the same lifecycle affordance ctrl+x reaches from the
//     roster ([App.doArchive]/[App.doKill]).
//
// # Cancel is the default on every dismissal path
//
// Esc from the choice list, and simply never answering (the client quits, the
// terminal closes, the connection drops), both land on cancel: the prompt is
// TUI-LOCAL state — nothing has been asked of the daemon at the point it opens,
// and it holds no pending server-side gate — so "dismissed" and "cancelled" are
// the same thing by construction, not by a cleanup path that has to run. The
// only two branches that touch the Supervisor are the two the user explicitly
// chooses. See TestCwdMissingCancelMutatesNothing and its siblings.
//
// # Never silently substitute a directory
//
// The prompt states the recorded directory that went missing, and — because
// a session's local context is cwd-scoped in ways that are invisible until they
// bite — states out loud that re-initing somewhere else REBASES that context.
// [cwdReinitWarning] is that copy; it is asserted by a named test
// (TestCwdMissingPromptWarnsAboutCwdScopedContext) rather than left to review.

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// cwdMissingNotifier is the narrow seam a [Supervisor] implementation may also
// satisfy to report "this session's recorded working directory is gone".
//
// It is declared HERE, in builtin types only, rather than imported: the
// implementations live on the daemon-backed bridge
// (daemonbridge.Supervisor.OnSessionCwdMissing) and on the in-process one
// (tuibridge.Adapter.OnSessionCwdMissing), and carrying either signature over
// would drag internal/daemon and internal/wirestream into internal/tui for two
// strings. [App] type-ASSERTS against it and degrades silently when it does not
// hold — the honest outcome for a Supervisor that genuinely cannot report this,
// not a description of any backend gofer ships: BOTH ship it. (The in-process
// backend has no session/load to fail on, but it resolves the same blank cwd
// against the same recorded directory — see internal/tuibridge/cwd.go — because
// the failure this reports is a missing DIRECTORY, not a wire error.)
//
// fn runs on a BACKGROUND goroutine (the reconstruction core's per-session load
// goroutine, or whichever goroutine called Resume) and must not block. Passing
// nil clears the registration.
type cwdMissingNotifier interface {
	OnSessionCwdMissing(fn func(sessionID, cwd string))
}

// cwdMissingSignal is one delivered "the recorded directory is gone" report:
// the session that could not be attached, and the recorded directory that no
// longer exists.
type cwdMissingSignal struct {
	sessionID string
	cwd       string
}

// cwdMissingMsg is [cwdMissingSignal] once it has crossed onto the Update loop
// (see [App.waitCwdMissing]). The channel hop is the whole point: the callback
// fires on a background goroutine, and touching App state from there would be a
// data race `go test -race ./...` catches.
type cwdMissingMsg cwdMissingSignal

// cwdMissingBuffer is how many signals the hand-off channel holds before the
// callback starts dropping them. A drop is harmless and deliberate: the prompt
// is modal and answers ONE session at a time, so a second signal arriving while
// the first is still on screen has nothing to render into ([App.applyCwdMissing]
// drops it for the same reason once it reaches the loop) — and the callback must
// never block a daemon goroutine waiting for a human. The buffer only exists so
// a signal raised before the listener re-arms (between one cwdMissingMsg being
// handled and the next read being issued) is not lost.
const cwdMissingBuffer = 8

// cwdDirsLoadedMsg carries the finished directory enumeration for the re-init
// picker. Like [filesLoadedMsg] it has no error field: every failure mode
// degrades to a shorter (possibly empty) list, and the free-text entry is
// always available, so "no candidates" is never a dead end.
type cwdDirsLoadedMsg struct{ dirs []string }

// cwdPromptStage is which of the prompt's two screens is showing.
type cwdPromptStage int

const (
	// cwdStageChoice is the three-way choice list — the state the prompt opens
	// in and the only one any dismissal path lands on.
	cwdStageChoice cwdPromptStage = iota
	// cwdStageDir is the directory picker option 1 opens: a free-text entry
	// line plus the enumerated candidates under this client's cwd.
	cwdStageDir
)

// The three choices, in render order. They are indices into the choice list's
// cursor as well as the "1"/"2"/"3" quick keys, so the order is load-bearing:
// cancel sits in the MIDDLE deliberately, between the two acting branches, so
// neither destructive option is adjacent to the other and an over-travelled
// cursor lands on the harmless one.
const (
	cwdChoiceReinit = iota
	cwdChoiceCancel
	cwdChoiceArchive
	cwdChoiceCount
)

// cwdReinitWarning is the load-bearing copy of this whole prompt: the reason
// "just point it somewhere that exists" is not a safe default.
//
// A session's local context is resolved against its cwd in four separate places
// — project config, user slash commands, skills, and file resolution — none of
// which announce themselves when they silently resolve differently. Re-initing
// elsewhere therefore REBASES the session's environment; it does not merely
// relocate it. That has to be said in the UI, at the moment of the choice, not
// only in a doc a user reading this prompt is not holding.
//
// Kept as a single unwrapped sentence and wrapped at render width by
// [cwdMissingPrompt.sections], so the assertion that it is on screen
// (TestCwdMissingPromptWarnsAboutCwdScopedContext) can normalise whitespace and
// still match regardless of where the wrap lands.
const cwdReinitWarning = "Warning: the session will load DIFFERENT local context there. " +
	"Project config (.gofer/config.json), user commands (<cwd>/.gofer/commands), skills, " +
	"and file resolution are all cwd-scoped — you are rebasing this session's environment, " +
	"not just pointing it somewhere that exists."

// cwdPromptMaxRows caps how much of the frame the prompt may take, the same way
// panelHeight caps the command panel: the screen underneath still has to show
// which session this is about.
const cwdPromptMaxRows = 20

// cwdMissingPrompt is the open prompt's state. Like every other TUI component
// here it is a pure value — every method returns an updated copy — so a fixed
// key sequence replays to the same rendered output in a golden test.
type cwdMissingPrompt struct {
	// sessionID is the session that could not be attached, and cwd the
	// RECORDED directory that no longer exists. cwd is rendered verbatim: it is
	// the daemon's own answer to "where was this recorded", and a client that
	// prettified it would be describing a path the user cannot go and look for.
	sessionID string
	cwd       string

	// title is the session's roster title, captured at open time so the prompt
	// says WHICH session it is about. The overview underneath is squeezed to a
	// few rows by a prompt this tall, so relying on the roster's own selection
	// to answer that would leave the question genuinely unanswerable on a short
	// terminal. Empty for a session the polled snapshot doesn't hold, which
	// falls back to the id.
	title string

	stage  cwdPromptStage
	cursor int // choice index in cwdStageChoice; candidate index (-1 = none) in cwdStageDir

	// entry is the typed directory path — the escape hatch for a directory the
	// enumeration below does not offer, mirroring [modelPickerView.entry]'s
	// role for an unlisted model id. It is a full [inputBuffer] rather than a
	// plain string because this prompt owns every key while it is open (no tab
	// host claims ←/→), so the shared editing keymap works here in full.
	entry inputBuffer

	// dirs is the bounded candidate list, absolute, sorted. loaded records that
	// the enumeration has ANSWERED — distinct from "answered with nothing",
	// which is a legitimate result the picker renders honestly.
	dirs   []string
	loaded bool
}

// newCwdMissingPrompt returns the prompt for one signal, in its choice stage
// with the cursor on the re-init option.
func newCwdMissingPrompt(sig cwdMissingSignal, title string) cwdMissingPrompt {
	return cwdMissingPrompt{sessionID: sig.sessionID, cwd: sig.cwd, title: title, cursor: cwdChoiceReinit}
}

// withDirs folds a finished enumeration in. A candidate list arriving while the
// user has already typed leaves the entry alone — it is text the user wrote,
// not a position in this data, the same rule [modelPickerView.withCatalog]
// follows.
func (p cwdMissingPrompt) withDirs(dirs []string) cwdMissingPrompt {
	p.dirs = dirs
	p.loaded = true
	if p.stage == cwdStageDir {
		p.cursor = clampInt(p.cursor, -1, len(p.candidates())-1)
	}
	return p
}

// candidates is the directory list actually offered: every enumerated directory
// whose path contains the typed text (case-insensitive), so the entry line
// doubles as a filter over the list it sits above.
func (p cwdMissingPrompt) candidates() []string {
	q := strings.ToLower(strings.TrimSpace(p.entry.String()))
	if q == "" {
		return p.dirs
	}
	out := make([]string, 0, len(p.dirs))
	for _, d := range p.dirs {
		if strings.Contains(strings.ToLower(d), q) {
			out = append(out, d)
		}
	}
	return out
}

// selectedDir returns what Enter commits in the directory stage: the
// highlighted candidate, else the typed entry. "" means "nothing to commit" —
// no highlight and an empty (or whitespace-only) entry — which Enter treats as
// a no-op rather than as a directory, because a blank cwd on the wire means
// "reopen where it was recorded" and that is exactly the state this prompt
// exists because of.
func (p cwdMissingPrompt) selectedDir() string {
	cands := p.candidates()
	if p.cursor >= 0 && p.cursor < len(cands) {
		return cands[p.cursor]
	}
	return strings.TrimSpace(p.entry.String())
}

// stepCandidate moves the candidate highlight by delta, with -1 ("none, the
// typed entry is what commits") as the position above the first row — the same
// drop-to-the-entry-line shape [modelPickerView.selectUp] has.
func (p cwdMissingPrompt) stepCandidate(delta int) cwdMissingPrompt {
	p.cursor = clampInt(p.cursor+delta, -1, len(p.candidates())-1)
	return p
}

// sections renders the prompt at width, split into its CONTEXT rows (the rule,
// the heading and the headline naming the session and the directory) and its
// ACTIONABLE rows (the choice list or the directory picker, plus the key hint
// under it). Every line is a finished terminal row — the warning is wrapped
// here, not by the caller — so [cwdMissingPrompt.height] can count them and
// [App.frameLayout] can budget against the count.
//
// The split exists for one reason: when the terminal cannot fit the whole
// prompt, something has to go, and it must never be the choices. A modal that
// renders a heading and a sentence while swallowing every key — with nothing on
// screen saying which keys it wants — is a wedge, not a prompt.
//
// compact additionally drops the re-init warning's sub-lines, which are the
// tallest thing here (four wrapped rows at 80 columns) and the only rows that
// are pure prose. They come off only when the actionable rows alone still do not
// fit, so on any ordinary terminal the warning renders in full — it is the copy
// the whole ruling turns on (see [cwdReinitWarning]).
func (p cwdMissingPrompt) sections(th theme.Theme, width int, compact bool) (head, body []string) {
	if width < 1 {
		width = 1
	}
	head = []string{strings.Repeat("─", width)}
	head = append(head, th.DangerStyle().Render(truncate("Session directory is gone", width)))
	head = append(head, wrapStyled(th.InkStyle(), p.headline(), width)...)
	head = append(head, "")
	if p.stage == cwdStageDir {
		return head, p.dirLines(th, width, compact)
	}
	return head, p.choiceLines(th, width, compact)
}

// headline states WHICH session, and WHICH directory went missing. Naming the
// directory is not decoration: it is how a user tells "I deleted that project"
// from "the volume isn't mounted", and the two have different answers.
func (p cwdMissingPrompt) headline() string {
	return fmt.Sprintf("%s was recorded in %s, which no longer exists — so it cannot be reopened where it was.",
		p.sessionLabel(), strconv.Quote(p.cwd))
}

// sessionLabel names the session in prose: its roster title where there is one,
// else its id. Never a bare "this session" — the prompt can appear over a
// roster the user was not looking at.
func (p cwdMissingPrompt) sessionLabel() string {
	if p.title != "" {
		return strconv.Quote(p.title)
	}
	return "Session " + p.sessionID
}

// choiceLines renders the three-way choice list through the shared vertical
// choice primitive ([choiceListLines]) every other prompt in this TUI answers
// through, so the caret, the gutter and the clamp are the same ones the
// approval and decision prompts use.
//
// The re-init warning renders as that row's sub-lines UNCONDITIONALLY, not only
// while the row is focused: a caveat a user has to navigate onto in order to
// read is a caveat that gets skipped.
func (p cwdMissingPrompt) choiceLines(th theme.Theme, width int, compact bool) []string {
	var warning []string
	if !compact {
		warning = indentStyled(th.WarnStyle(), cwdReinitWarning, "    ", width)
	}
	rows := []choiceRow{
		{
			leader:   "1 ",
			label:    "Re-init this session in a new directory…",
			sublines: warning,
		},
		{leader: "2 ", label: "Cancel — leave this session untouched (it stays unattachable)."},
		{leader: "3 ", label: "Archive / delete this session — its journal is kept."},
	}
	out := choiceListLines(th, rows, p.cursor, width)
	out = append(out, "")
	return append(out, th.MutedStyle().Render(truncate("↑/↓ move · 1-3 pick · enter choose · esc cancel", width)))
}

// dirLines renders the directory stage: the same warning (still on screen at
// the moment of commitment, which is the point of it), the free-text entry, and
// the filtered candidate list.
func (p cwdMissingPrompt) dirLines(th theme.Theme, width int, compact bool) []string {
	var out []string
	if !compact {
		out = indentStyled(th.WarnStyle(), cwdReinitWarning, "  ", width)
		out = append(out, "")
	}
	out = append(out, truncate("Directory: "+p.entry.Render("▏"), width))

	cands := p.candidates()
	switch {
	case !p.loaded:
		out = append(out, th.MutedStyle().Render(truncate("  finding directories…", width)))
	case len(cands) == 0:
		out = append(out, th.MutedStyle().Render(truncate("  no directories found here — type a path instead", width)))
	default:
		for i, d := range cands {
			if len(out) >= cwdPromptMaxRows-2 {
				break
			}
			marker := "  "
			line := marker + displayHome(d)
			if i == p.cursor {
				line = th.AccentStyle().Render(choiceCaret + " " + displayHome(d))
			}
			out = append(out, truncate(line, width))
		}
	}
	out = append(out, "")
	return append(out, th.MutedStyle().Render(truncate("type a path · ↑/↓ browse · enter re-init here · esc back", width)))
}

// render draws the prompt at the given size, clipped to at most
// [cwdPromptMaxRows] rows. It takes the theme explicitly because the prompt is
// a pure value that holds none of its own — unlike the panel views, which are
// constructed per open and can capture one.
func (p cwdMissingPrompt) render(th theme.Theme, width, height int) string {
	return strings.Join(p.frame(th, width, height), "\n")
}

// height reports how many rows [cwdMissingPrompt.render] will take at width
// given all the room it wants, so [App.frameLayout] reserves exactly that many
// instead of always the worst case — the same accounting [commandPanel.Height]
// does for the panel. frameLayout may hand render LESS than this on a short
// terminal, which is the case [cwdMissingPrompt.frame] exists to survive.
func (p cwdMissingPrompt) height(th theme.Theme, width int) int {
	return len(p.frame(th, width, cwdPromptMaxRows))
}

// frame is the prompt's rows clipped to at most n (and never beyond
// [cwdPromptMaxRows]), dropping the LEAST actionable content first.
//
// The order it sheds in — warning sub-lines, then the context rows from the
// bottom up, then finally the actionable rows themselves — is the whole point.
// Clipping the row list flat (lines[:n], which this used to do) starves from the
// wrong end: the rule, the heading and a two-line headline are six rows before
// the first choice, so a prompt given five rows rendered a heading, half a
// sentence, and NOT ONE of the three options — while still swallowing every key
// press, because the prompt is modal whether or not its choices are on screen.
// The user could see something had gone wrong and had no way to answer it.
func (p cwdMissingPrompt) frame(th theme.Theme, width, n int) []string {
	if n > cwdPromptMaxRows {
		n = cwdPromptMaxRows
	}
	if n <= 0 {
		return nil
	}
	head, body := p.sections(th, width, false)
	if len(body) > n {
		head, body = p.sections(th, width, true)
	}
	if len(body) >= n {
		return body[:n]
	}
	// At least one row is left for the context block. Keep its FIRST rows: the
	// rule and the "Session directory is gone" heading say what this is, and the
	// headline's later wrap rows are the most expendable thing on screen.
	room := n - len(body)
	if room >= len(head) {
		return append(head, body...)
	}
	return append(head[:room:room], body...)
}

// wrapStyled word-wraps s to width and applies style to each resulting row.
// Styling AFTER the wrap is what keeps the arithmetic honest: [wrap] measures
// display width, and wrapping already-styled text would count the ANSI escapes
// into the line's own budget on some inputs.
func wrapStyled(style lipgloss.Style, s string, width int) []string {
	rows := wrap(s, width)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = style.Render(r)
	}
	return out
}

// indentStyled is [wrapStyled] with every row prefixed by indent — the shape a
// choiceRow's sub-lines need, since [choiceListLines] draws them verbatim.
func indentStyled(style lipgloss.Style, s, indent string, width int) []string {
	rows := wrap(s, width-len(indent))
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = style.Render(indent + r)
	}
	return out
}

// ---------------------------------------------------------------------------
// App wiring
// ---------------------------------------------------------------------------

// registerCwdMissing installs the background handler that turns a Supervisor's
// cwd-missing report into a message on the Update loop, when the Supervisor is
// one that can raise it at all.
//
// The closure captures the CHANNEL, never the App: it runs on a daemon-side
// goroutine, so reading or writing App state from it would be a data race. It
// also never blocks — a full buffer drops the signal (see [cwdMissingBuffer]),
// because the alternative is parking a daemon goroutine until a human answers a
// prompt.
func (a *App) registerCwdMissing(sup Supervisor) {
	n, ok := sup.(cwdMissingNotifier)
	if !ok {
		// A Supervisor that cannot report this failure at all — a test double,
		// or a future backend. Not an error and not a degraded mode: there is
		// nothing to listen for. Both backends gofer ships DO implement it.
		return
	}
	ch := make(chan cwdMissingSignal, cwdMissingBuffer)
	a.cwdMissing = ch
	n.OnSessionCwdMissing(func(sessionID, cwd string) {
		select {
		case ch <- cwdMissingSignal{sessionID: sessionID, cwd: cwd}:
		default:
		}
	})
}

// waitCwdMissing blocks for the next cwd-missing signal — the ONLY place this
// package reads that channel, and it does so inside a tea.Cmd (its own
// goroutine) rather than on the Update loop. It returns nil when no notifier
// was registered, which makes [App.Init]'s tea.Batch collapse back to the bare
// roster fetch on every non-daemon backend (Batch drops nil commands).
func (a App) waitCwdMissing() tea.Cmd {
	ch := a.cwdMissing
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		sig, ok := <-ch
		if !ok {
			return nil
		}
		return cwdMissingMsg(sig)
	}
}

// applyCwdMissing opens the prompt for a delivered signal.
//
// It takes the screen for itself: any open command panel is closed and the
// autocomplete menu dropped, so the prompt is unambiguously the thing being
// answered (it is dispatched ahead of both in [App.Update]). A signal naming no
// session is ignored rather than opening a prompt that can act on nothing, and
// so is one arriving while a prompt is ALREADY open: the prompt is modal and
// answers one session at a time, so adopting the newcomer would discard the
// stage and any directory the user has typed, and silently re-point the answer
// at a different session. The dropped signal costs nothing that is not
// recoverable — attaching that session again raises it again (see
// [wirestream.Reconstructor]'s cwd-missing retry).
//
// It also returns to the OVERVIEW, and TEARS DOWN the subscription the aborted
// attach opened. The roster-Enter path has already switched to the attach
// screen by the time this signal lands — the load is asynchronous — so leaving
// it there would draw the prompt over an empty transcript, which reads as a
// session that opened and had nothing in it. The attach did not happen; the
// frame should not imply it did, and neither should the App's state. Dropping
// the subscription is what makes a SECOND Enter on the same roster row retry
// at all: [App.enter] is a no-op while sessID is still subscribed, so a
// leftover stream for a session that never opened would turn the retry into a
// silent nothing.
func (a App) applyCwdMissing(msg cwdMissingMsg) App {
	if msg.sessionID == "" || a.cwdPrompt != nil {
		return a
	}
	if msg.sessionID == a.sessID {
		if a.sub != nil {
			a.sub.Close()
		}
		if a.decSub != nil {
			a.decSub.Close()
		}
		a.sub, a.decSub, a.sessID = nil, nil, ""
		a.sess = New(a.theme)
	}
	title := ""
	if s, ok := a.over.SessionByID(msg.sessionID); ok {
		title = s.Title
	}
	p := newCwdMissingPrompt(cwdMissingSignal(msg), title)
	a.cwdPrompt = &p
	a.panel = nil
	a.menu = commandMenu{}
	a.scr = screenOverview
	a.scroll = 0
	a.clearStatus()
	return a
}

// applyCwdDirsLoaded folds a finished directory enumeration into the open
// prompt. A prompt DISMISSED while the walk was in flight drops the result, the
// same rule [App.applyModelsLoaded]/[App.applySessionsListed] follow: the next
// open re-enumerates, and a stale slice must never resurrect a dismissed
// prompt.
func (a App) applyCwdDirsLoaded(msg cwdDirsLoadedMsg) App {
	if a.cwdPrompt == nil {
		return a
	}
	p := a.cwdPrompt.withDirs(msg.dirs)
	a.cwdPrompt = &p
	return a
}

// cwdDirCandidatesCmd enumerates candidate directories OFF the Update loop,
// through the very same bounded walk the `@` file mention uses
// ([listFileCandidates] — git-first, .gitignore-honoring, capped by
// tui.file_mention_max_entries / tui.file_mention_max_depth and by the shell
// timeout). Reusing it rather than writing a second walk is the point: one
// enumeration, one set of bounds, one place a "this hangs in a huge tree" bug
// could ever live.
func (a App) cwdDirCandidatesCmd() tea.Cmd {
	cwd := a.commandEnv.Cwd
	maxEntries, maxDepth := a.fileMentionLimits()
	timeout := a.shellTimeout()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return cwdDirsLoadedMsg{dirs: directoriesOf(listFileCandidates(ctx, cwd, maxEntries, maxDepth), cwd, maxEntries)}
	}
}

// directoriesOf folds a cwd-relative file listing into the sorted, unique set of
// ABSOLUTE directories holding those files, plus base itself.
//
// Absolute, because this list feeds a cwd that goes on the wire: ACP requires an
// absolute path, and a relative one would be resolved against the DAEMON's
// working directory — a different machine's idea of "here", which is precisely
// the silent substitution this whole prompt exists to prevent.
//
// base is normalized before anything is derived from it, rather than trusted:
// it comes from [CommandEnv.Cwd], which is free-form enough to hold a "~/…"
// string, and seeding from that verbatim would emit candidates that are neither
// absolute nor tilde-free — contradicting the contract above. [resolveChosenDir]
// would still expand whichever one the user picked, so this was never a wire
// bug; it made the guarantee true HERE too, so the invariant is not left resting
// on a single downstream caller. A base that cannot be made absolute yields no
// candidates at all: the free-text entry still works, and a guessed root is the
// one thing this prompt must never offer.
func directoriesOf(paths []string, base string, limit int) []string {
	base = expandHome(strings.TrimSpace(base))
	if base == "" {
		return nil
	}
	if !filepath.IsAbs(base) {
		abs, err := filepath.Abs(base)
		if err != nil {
			return nil
		}
		base = abs
	}
	base = filepath.Clean(base)
	seen := map[string]bool{base: true}
	out := []string{base}
	for _, p := range paths {
		dir := path.Dir(filepath.ToSlash(p))
		if dir == "." || dir == "/" || dir == "" {
			continue
		}
		abs := filepath.Join(base, filepath.FromSlash(dir))
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out
}

// resolveChosenDir turns what the user picked or typed into the cwd actually
// sent to the daemon. Its post-condition is the one [directoriesOf] states for
// the candidate list it sits beside: whatever comes back is ABSOLUTE, or it is
// empty. Nothing relative, and no unexpanded "~", ever reaches the wire.
//
// Every relative frame of reference is resolved HERE, on the client, because
// each of them means "here" to the USER and the daemon's own idea of "here" is
// a different machine's:
//
//   - a leading "~" expands against THIS client's home ([expandHome]). The
//     daemon would expand it against its own (internal/daemon's normalizeCwd),
//     which for a remote daemon — or one running as another user — is a
//     directory the user never named. It still validates there, so the
//     substitution would be silent and successful, which is the failure mode
//     this whole prompt exists to prevent;
//   - a relative path joins onto base, the client cwd the candidate list is
//     relative to and what the user is typing against. base is itself expanded
//     first: it comes from [CommandEnv.Cwd], which is free-form enough to hold a
//     "~/…" string, and joining onto that would only move the tilde further into
//     the middle of the path;
//   - with no usable base, filepath.Abs anchors it to the process's own working
//     directory — still this client's "here", never the daemon's.
//
// It never invents a directory: an empty input stays empty, and the caller
// treats that as "nothing to commit".
func resolveChosenDir(input, base string) string {
	s := expandHome(strings.TrimSpace(input))
	if s == "" {
		return ""
	}
	if filepath.IsAbs(s) {
		return filepath.Clean(s)
	}
	if b := expandHome(strings.TrimSpace(base)); filepath.IsAbs(b) {
		return filepath.Join(b, s)
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return ""
	}
	return abs
}

// handleCwdPromptKey routes one key press to the open prompt. [App.Update]
// calls it AHEAD of every other overlay: the prompt is modal, it opened by
// itself in response to something the user did not type, and the three answers
// it wants are unambiguous.
//
// Esc is the dismissal path, and it always lands on cancel: from the directory
// stage it steps BACK to the choice list (so a mistyped path costs a keystroke,
// not the whole prompt — the two-stage escape [modelPickerView.handleEscape]
// established), and from the choice list it cancels outright. Neither touches
// the Supervisor.
func (a App) handleCwdPromptKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	if key.Mod.Contains(tea.ModCtrl) && key.Code == 'c' {
		// Double-tap confirm (gofer#314), like every other overlay that claims
		// ctrl+c. Esc below is still the un-confirmed way out, so requiring the
		// second press cannot strand a user who only meant to dismiss this.
		return a.confirmQuit()
	}
	if a.cwdPrompt.stage == cwdStageDir {
		return a.handleCwdDirKey(msg)
	}
	return a.handleCwdChoiceKey(msg)
}

// handleCwdChoiceKey handles the three-way choice list.
func (a App) handleCwdChoiceKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Code == tea.KeyEscape:
		return a.cancelCwdPrompt(), nil

	case key.Code == tea.KeyUp:
		p := *a.cwdPrompt
		p.cursor = stepChoiceCursor(p.cursor, -1, cwdChoiceCount)
		a.cwdPrompt = &p
		return a, nil

	case key.Code == tea.KeyDown:
		p := *a.cwdPrompt
		p.cursor = stepChoiceCursor(p.cursor, 1, cwdChoiceCount)
		a.cwdPrompt = &p
		return a, nil

	case key.Text == "1":
		return a.takeCwdChoice(cwdChoiceReinit)
	case key.Text == "2":
		return a.takeCwdChoice(cwdChoiceCancel)
	case key.Text == "3":
		return a.takeCwdChoice(cwdChoiceArchive)

	case key.Code == tea.KeyEnter:
		return a.takeCwdChoice(a.cwdPrompt.cursor)
	}
	return a, nil
}

// takeCwdChoice applies one of the three answers.
func (a App) takeCwdChoice(choice int) (tea.Model, tea.Cmd) {
	switch choice {
	case cwdChoiceReinit:
		p := *a.cwdPrompt
		p.stage = cwdStageDir
		p.cursor = -1 // the typed entry commits until ↓ moves onto a candidate
		a.cwdPrompt = &p
		return a, a.cwdDirCandidatesCmd()
	case cwdChoiceArchive:
		return a.archiveCwdMissingSession()
	default:
		// Cancel, and anything an out-of-range cursor could ever be: the safe
		// branch is the one that mutates nothing.
		return a.cancelCwdPrompt(), nil
	}
}

// cancelCwdPrompt dismisses the prompt back to the overview, having asked the
// Supervisor for NOTHING. The session stays exactly as it was — still on the
// roster, still unattachable — which is the honest outcome: cancelling a remedy
// does not fix the thing it was offered for, and pretending otherwise (by
// hiding the row, say) would be worse than the error this replaced.
func (a App) cancelCwdPrompt() App {
	a.cwdPrompt = nil
	a.scr = screenOverview
	a.scroll = 0
	return a
}

// archiveCwdMissingSession takes the third choice, dispatching the SAME
// lifecycle op the roster's ctrl+x confirm does for the same session state
// ([App.confirmDestroy]): archive for a session at rest, kill for a live one.
// A session the polled roster snapshot doesn't hold archives — a session that
// could not be loaded is by definition not running, so there is no live runner
// to kill.
func (a App) archiveCwdMissingSession() (tea.Model, tea.Cmd) {
	id := a.cwdPrompt.sessionID
	a.cwdPrompt = nil
	a.scr = screenOverview
	a.scroll = 0
	if s, ok := a.over.SessionByID(id); ok && s.Status != StatusFinished && s.Status != StatusIdle {
		return a, a.doKill(id)
	}
	return a, a.doArchive(id)
}

// handleCwdDirKey handles the directory stage: Enter commits, Esc steps back to
// the choice list, ↑/↓ browse the candidates, and everything else edits the
// free-text entry through the shared input keymap.
func (a App) handleCwdDirKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch key.Code {
	case tea.KeyEscape:
		p := *a.cwdPrompt
		p.stage = cwdStageChoice
		p.cursor = cwdChoiceReinit
		a.cwdPrompt = &p
		return a, nil

	case tea.KeyUp:
		p := a.cwdPrompt.stepCandidate(-1)
		a.cwdPrompt = &p
		return a, nil

	case tea.KeyDown:
		p := a.cwdPrompt.stepCandidate(1)
		a.cwdPrompt = &p
		return a, nil

	case tea.KeyEnter:
		return a.commitCwdReinit()
	}

	p := *a.cwdPrompt
	if buf, ok := applyInputKey(p.entry, key); ok {
		p.entry = buf
		// Typing re-filters the candidate list, so a highlight held over from
		// the previous filter would point at an unrelated row. Drop it: what is
		// on screen as the entry is what Enter commits, the same rule
		// [modelPickerView.typeEntry] applies.
		p.cursor = -1
		a.cwdPrompt = &p
	}
	return a, nil
}

// commitCwdReinit sends the user's chosen directory as an EXPLICIT, non-blank
// cwd — which is the whole mechanism: the daemon cannot tell a client's echo of
// the journal from a user's choice, so gofer#326 made blank mean "where it was
// recorded" and non-blank mean "the user said so". A directory that does not
// exist is therefore still a hard -32602 rejection here, surfacing on the
// ordinary status line, and that is correct: the user named it.
//
// Committing nothing (no candidate highlighted and an empty entry) is a no-op
// with the prompt left open — never a blank cwd, which would ask the daemon to
// reopen the session in the very directory that is missing.
//
// Where the session is being reopened is STATED, not implied: the status note
// names the directory before the round trip resolves either way.
func (a App) commitCwdReinit() (tea.Model, tea.Cmd) {
	dir := resolveChosenDir(a.cwdPrompt.selectedDir(), a.commandEnv.Cwd)
	if dir == "" {
		return a, nil
	}
	id := a.cwdPrompt.sessionID
	a.cwdPrompt = nil
	a.setStatus(sevOK, fmt.Sprintf("Reopening session in %s.", displayHome(dir)))
	return a, a.doResume(id, dir)
}
