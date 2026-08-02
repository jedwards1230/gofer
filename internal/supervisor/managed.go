package supervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/decision"
)

// managed is one live session's supervisor-side bookkeeping: the Session it
// drives, its run-state and prompt queue, and the plumbing (baseCtx/cancel,
// submitCh, done) its dedicated pump goroutine uses.
//
// Lock discipline: mu guards every field below it. The pump goroutine and
// every Supervisor method that touches a managed session's state hold mu
// only for the bookkeeping itself — never across the blocking Session calls
// (Prompt, Close), across waiting on done, or across notify (which snapshots
// the whole roster). Supervisor methods that also need the roster lock (mu on
// *Supervisor) always take it before mu here, so the two locks have one fixed
// order and cannot deadlock.
type managed struct {
	sess    Session
	id      string
	project string
	model   string
	cwd     string
	// parentID/agent/depth are this session's subagent link (see [sessionMeta]):
	// the spawning session's id, the agent identity its tool events are stamped
	// with, and its depth in the tree. Set once in newManaged — from Create's
	// resolved options or, on resume, from the on-disk sidecar — and never
	// mutated afterward, so (like id/project/cwd above) they are read without
	// holding mu.
	parentID  string
	agent     string
	depth     int
	createdAt time.Time
	clock     func() time.Time
	// notify pushes a fresh roster snapshot to WatchRoster subscribers. The
	// pump calls it after each run-state transition; it must never be called
	// while holding mu (it snapshots every session, taking each one's mu).
	notify func()

	// baseCtx/baseCancel bound the session's entire live lifetime. Kill,
	// Archive, and Close all stop the session by cancelling baseCtx — which
	// both interrupts any in-flight turn (turnCtx is derived from it) and
	// wakes the pump goroutine out of its idle wait so it can exit.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	// done is closed by the pump goroutine when it returns. Kill/Archive/
	// Close wait on it after cancelling baseCtx, so a session is fully
	// stopped (no turn still running against it) before its lifecycle event
	// is emitted and it is closed.
	done chan struct{}
	// gate is this session's approval Gate: the guard's Await blocks on it, and
	// [Supervisor.Reply] routes a human's inbound reply into it. One per session,
	// never nil.
	gate *loop.Gate
	// decisions is this session's structured-decision Gate: its ask_user tool
	// blocks on it, [Supervisor.SubscribeDecisions] watches it, and
	// [Supervisor.AnswerDecision] resolves through it. One per session, never
	// nil, and — like gate — immutable after construction, so it needs no lock.
	decisions *decision.Gate
	// permDone is closed by the watchPermissions goroutine when it returns, so
	// stop joins it alongside the pump — leaving no subscription goroutine
	// behind on shutdown.
	permDone chan struct{}
	// decisionOnce guards starting the decision watcher, which two racing
	// callers can reach: [Supervisor.register] (for a session created after a
	// relay was installed) and [Supervisor.SetDecisionRelay] (for a session that
	// already existed when one was). Exactly one wins; the loser is a no-op.
	decisionOnce sync.Once
	// decisionStarted reports that decisionOnce actually started a watcher, so
	// stop knows whether decisionDone will ever close. Set INSIDE the once,
	// before the goroutine is spawned.
	decisionStarted atomic.Bool
	// decisionDone is closed by the watchDecisions goroutine when it returns,
	// joined by stop the same way permDone is.
	decisionDone chan struct{}
	// teardown is the func returned by Config.OnRegister (nil if unset or if
	// OnRegister itself returned nil), joined by stop after the pump and
	// permission watcher have both exited. Set once, in newManaged, before m
	// is published into the roster — never mutated afterward, so no lock is
	// needed to read it in stop.
	teardown func()
	// submitCh wakes an idle pump when Send enqueues a prompt. Buffered
	// size 1 and sent to non-blockingly: multiple submits while the pump is
	// busy coalesce into one wakeup, which is fine — the pump drains the
	// whole queue once woken, not one item per wakeup.
	submitCh chan struct{}

	mu sync.Mutex
	// effort is the session's current reasoning effort ("", "low", "medium",
	// "high"), seeded from the runner's construction-time
	// Params.Thinking.Effort and updated by [Supervisor.SetEffort]. It is
	// bookkeeping only — the runner owns the value it actually sends — kept
	// here for the same reason model is: the [Session] interface exposes no
	// accessor, and info must be able to report it.
	effort string
	// state is the session's current pump run-state, read by info (which
	// derives SessionStatus) and by Archive to reject archiving a running
	// session.
	state runState
	// resumed marks a session that came back live off disk (Supervisor.Resume)
	// and has NOT been prompted since. While set, an idle session with an empty
	// queue derives StatusIdle rather than StatusNeedsInput, so merely opening a
	// reloaded row does not present it as awaiting the user (see info). Cleared
	// the moment the first prompt is enqueued (see enqueue) — from then on the
	// session has really done work and derives status normally. A freshly
	// CREATED session leaves this false, so a new empty session still reads as
	// NeedsInput, exactly as before.
	resumed bool
	// updated is bumped on every run-state transition (idle⇄running), which
	// coincides with turn dispatch and turn completion (turn.finished).
	updated time.Time
	// title is the first prompt's snippet, captured once when the first
	// prompt is enqueued; info falls back to the project slug when it is "".
	title string
	// queue holds prompts not yet dispatched, in submit order. queue[0] is
	// the next prompt the pump will run.
	queue []string
	// turnCancel cancels the in-flight turn's context; nil when idle.
	// Interrupt calls it if set.
	turnCancel context.CancelFunc
	// closing is set by Kill/Archive/Close before they cancel baseCtx. The
	// pump checks it before dispatching the next queued prompt so a session
	// caught idle-with-a-queued-prompt at the exact moment it is
	// archived/killed does not race a new turn into existence after the
	// closing decision was made — see Archive's doc comment for the race
	// this closes. Send also checks it, so a prompt cannot be queued onto a
	// session that has already decided to stop.
	closing bool
	// pending is the live count of outstanding permission requests: +1 on this
	// session's event.PermissionRequested, −1 on event.PermissionResolved,
	// maintained by watchPermissions and surfaced as SessionInfo.Pending.
	pending int
	// pendingPerms holds the SAME outstanding requests pending counts, keyed by
	// call id and carrying each one's full payload (tool, spec, decision
	// trace). The count alone answers "how many?"; this answers "why was THAT
	// one gated?" — the question [Supervisor.ExplainPermission] exists for, and
	// which a daemonless TUI has no other source for (the daemon path answers
	// it from its own retained requests; see internal/daemon's pendingPerms).
	// Added and removed at the exact two points pending is adjusted, under the
	// same mutex, so the two can never disagree about what is outstanding.
	pendingPerms map[string]event.PermissionRequested
	// lastErr is the most recent turn's Prompt error, kept for diagnostics
	// only (see [Supervisor.LastError]). It is a snapshot, not a delivery
	// mechanism: the pump emits a session.error onto the session's own stream
	// for every non-cancelled failure, and that emit — not this field — is
	// what reaches subscribers. Provider/loop errors additionally surface as
	// session.error from inside the loop, but a journal write failure does
	// not, which is why the pump's emit is unconditional rather than filtered
	// to a particular error class. A cancelled turn is expected, so the pump
	// never treats a Prompt error as a supervisor-level failure.
	lastErr error

	// compaction resolves the LIVE automatic-compaction policy (see
	// [config.Compaction]), called fresh at the end of every settled turn
	// (see pump) rather than sampled once at construction — so a
	// `compaction.disabled`/threshold edit to config.json, or the /config
	// panel's future equivalent, takes effect on the very next turn with no
	// restart, the same live-reload shape [Config.PermissionMode] and
	// [Config.Permissions] already follow. Never nil after newManaged.
	compaction func() config.Compaction

	// reportParent delivers THIS session's one child→parent report (see
	// [Subagents.Report]), or nil when there is nothing to report to: a root
	// session, or a child on a gofer that never opted into subagents. It is set
	// once by [Supervisor.register] BEFORE the pump goroutine starts and never
	// written again, so the pump reads it without a lock — the goroutine start
	// is the publish.
	reportParent func(ctx context.Context, parentID, text string) error
	// reportOnce bounds the report to exactly one delivery per session. See
	// [managed.reportToParentOnce] for why one, and not one per settled turn.
	reportOnce sync.Once
}

// newManaged builds a managed session ready to register: idle, empty queue,
// its own cancellable base context. If onRegister is non-nil, it is invoked
// here — with the session, before m is returned to register for roster
// publish — and its returned teardown (if any) is stashed on m for stop to
// join later. Calling it here, rather than after publish, closes the race
// where a concurrent Kill/Archive could otherwise observe a live session
// with no teardown stashed yet (see Config.OnRegister's doc).
func newManaged(sess Session, model, effort string, now time.Time, clock func() time.Time, notify func(), cwd string, gate *loop.Gate, decisions *decision.Gate, meta sessionMeta, resumed bool, onRegister func(sess Session) (stop func()), compaction func() config.Compaction) *managed {
	ctx, cancel := context.WithCancel(context.Background())
	m := &managed{
		sess:         sess,
		id:           sess.ID(),
		project:      filepath.Base(filepath.Dir(sess.JournalPath())),
		model:        model,
		effort:       effort,
		cwd:          cwd,
		parentID:     meta.ParentID,
		agent:        meta.Agent,
		depth:        meta.Depth,
		createdAt:    now,
		updated:      now,
		clock:        clock,
		notify:       notify,
		baseCtx:      ctx,
		baseCancel:   cancel,
		done:         make(chan struct{}),
		gate:         gate,
		decisions:    decisions,
		permDone:     make(chan struct{}),
		decisionDone: make(chan struct{}),
		submitCh:     make(chan struct{}, 1),
		state:        stateIdle,
		resumed:      resumed,
		compaction:   compaction,

		pendingPerms: make(map[string]event.PermissionRequested),
	}
	if onRegister != nil {
		m.teardown = onRegister(sess)
	}
	return m
}

// info snapshots m under its own lock into a live [SessionInfo], deriving
// Status from the pump run-state and queue depth and reading a fresh cost
// tally from the session.
func (m *managed) info() SessionInfo {
	report := m.sess.Cost()
	// LastUsage/ContextWindow are read the same way Cost is — a Session call
	// outside m.mu, per this file's lock discipline — so a live roster row
	// carries the pressure figures [SessionInfo.LastUsage]/[SessionInfo.ContextWindow]
	// document without this snapshot ever blocking on the pump.
	lastUsageModel, lastUsage, _ := m.sess.LastUsage()
	var contextWindow int
	if info, ok := provider.Lookup(lastUsageModel); ok {
		contextWindow = info.ContextWindow
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	status := StatusNeedsInput
	switch {
	case m.state == stateRunning || len(m.queue) > 0:
		status = StatusWorking
	case m.resumed && m.pending == 0:
		// Reloaded off disk and not prompted since, with no pending request: at
		// rest, not awaiting the user. A genuine pending permission keeps it
		// awaiting (pending != 0 falls through to StatusNeedsInput), and the
		// TUI's effectiveStatus applies the same rule for a resumed row that
		// carries one — so a real prompt is never hidden behind StatusIdle.
		status = StatusIdle
	}
	title := m.title
	if title == "" {
		title = m.project
	}
	return SessionInfo{
		ID:            m.id,
		Title:         title,
		Status:        status,
		Model:         m.model,
		Effort:        m.effort,
		Cost:          report.Cost,
		Usage:         report.Usage,
		Pending:       m.pending,
		Created:       m.createdAt,
		Updated:       m.updated,
		Project:       m.project,
		JournalPath:   m.sess.JournalPath(),
		Queued:        len(m.queue),
		Live:          true,
		Cwd:           m.cwd,
		ParentID:      m.parentID,
		Agent:         m.agent,
		Depth:         m.depth,
		LastUsage:     lastUsage,
		ContextWindow: contextWindow,
	}
}

// seedTitle sets the session's title from a resume-time journal derivation (its
// first user message; see firstUserSnippet), but only when no title is set yet —
// a resumed session is idle with no fresh prompt, so its title is empty and this
// restores the label the offline row showed. A no-op for an empty derivation or
// once a prompt has already captured a title in [managed.enqueue]. Locked like
// every other m.title access, so it stays race-clean against enqueue and info.
func (m *managed) seedTitle(title string) {
	if title == "" {
		return
	}
	m.mu.Lock()
	if m.title == "" {
		m.title = title
	}
	m.mu.Unlock()
}

// seedUpdated restores a resumed session's last-activity time from its journal's
// last entry, so the overview age reflects the last real conversation event
// rather than the moment the session was reopened. newManaged seeds updated to
// the resume time as a fallback; this overwrites it with the journaled truth, so
// merely resuming/attaching to a row no longer flips its age to "now" — it keeps
// the same last-activity the offline row showed (see diskSessionInfo, which
// derives Updated from the same last entry's Time).
//
// It is a no-op for a zero time (an unreadable or empty journal — the resume-time
// fallback then stands, matching the offline row that reports a zero Updated),
// and, like seedTitle, only touches a still-resumed session: once the first
// prompt clears resumed in enqueue, the session has done real work and the pump
// owns updated from there on, so this must not drag it back to a stale journal
// time. Locked like every other m.updated access.
func (m *managed) seedUpdated(t time.Time) {
	if t.IsZero() {
		return
	}
	m.mu.Lock()
	if m.resumed {
		m.updated = t
	}
	m.mu.Unlock()
}

// enqueue appends text to the pump's queue and wakes it. It captures the
// session title from the first prompt that yields a non-empty snippet, set once
// and never overwritten thereafter, and — on that first capture only — emits a
// [event.SessionInfoUpdated] onto the session's stream so ACP peers observe the
// derived title live (it projects to an ACP session_info_update for free). A
// re-prompt with the same or different text never re-emits: the title is
// already set, so newTitle stays "". It returns ErrNotLive if the session has
// already begun closing.
func (m *managed) enqueue(text string) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return ErrNotLive
	}
	// The session is being prompted: it is no longer a resumed-but-untouched
	// row, so it derives status normally from here on (StatusWorking now, then
	// StatusNeedsInput once this turn settles with an empty queue).
	m.resumed = false
	var newTitle string
	if m.title == "" {
		if t := snippet(text); t != "" {
			m.title = t
			newTitle = t
		}
	}
	m.queue = append(m.queue, text)
	m.mu.Unlock()

	// Emit outside m.mu: a must-deliver publish can block on backpressure, and
	// the lock discipline in this file's doc forbids blocking Session calls
	// under m.mu. Guarded by newTitle != "" so a whitespace-only first prompt
	// (empty snippet) and every subsequent prompt emit nothing.
	if newTitle != "" {
		m.sess.Emit(event.NewSessionInfoUpdated(m.id, newTitle))
	}

	select {
	case m.submitCh <- struct{}{}:
	default:
	}
	m.notify()
	return nil
}

// pump is m's dedicated goroutine: it dispatches queued prompts one at a
// time, blocking on Prompt (never under m.mu), until baseCtx is cancelled or
// it observes closing set. It closes m.done on return and calls notify on
// every run-state transition.
func (m *managed) pump() {
	defer close(m.done)
	for {
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			<-m.baseCtx.Done()
			return
		}
		if len(m.queue) == 0 {
			changed := m.state != stateIdle
			m.state = stateIdle
			if changed {
				m.updated = m.clock()
			}
			m.mu.Unlock()
			if changed {
				m.notify()
			}
			select {
			case <-m.submitCh:
				continue
			case <-m.baseCtx.Done():
				return
			}
		}

		text := m.queue[0]
		m.queue = m.queue[1:]
		turnCtx, cancel := context.WithCancel(m.baseCtx)
		m.turnCancel = cancel
		m.state = stateRunning
		m.updated = m.clock()
		m.mu.Unlock()
		m.notify()

		err := m.sess.Prompt(turnCtx, text)

		// Failure-triggered compaction: the SECOND, INDEPENDENT trigger, for
		// the one case the post-flight threshold check below structurally
		// cannot see (see recoverFromContextOverflow). Whatever the recovery
		// settles on REPLACES err, so the rest of the loop body — the emit,
		// the threshold check, the notify — treats the recovered turn exactly
		// like any other: a successful retry reads as a clean turn, and a
		// failed one as an ordinary turn failure.
		//
		// A plain `if`, not a `for`: that is the whole retry bound (see the
		// function's doc), and it is why the original overflow can never be
		// emitted twice — err no longer holds it once we are past here.
		//
		// It takes over retiring turnCtx, deliberately: it installs its OWN
		// fresh turnCancel under mu BEFORE cancelling this one, so m.turnCancel
		// never transiently names a spent context (see its doc for why that
		// window matters now that real work sits behind it).
		if errors.Is(err, provider.ErrContextOverflow) {
			err = m.recoverFromContextOverflow(text, err, cancel)
		}
		// Idempotent — the recovery above already called it on the overflow
		// path. Unconditional here so the ordinary path always retires turnCtx.
		cancel()

		m.mu.Lock()
		m.lastErr = err
		m.turnCancel = nil
		m.updated = m.clock()
		m.mu.Unlock()

		// Surface the failure on the session's own stream so every observer —
		// TUI, ACP peers, telemetry — sees it. lastErr above is only a
		// diagnostic snapshot and nothing reads it, so this emit is the only
		// thing that actually reaches a user. It matters most for a journal
		// write failure: the SDK reports that solely as Prompt's return value,
		// never as an event of its own, so dropping it here would let a session
		// keep serving a normal-looking transcript while entries are missing
		// from the JSONL — surfacing only later, on resume, as agent amnesia.
		//
		// A cancelled turn is the expected outcome of Interrupt/Kill/Archive,
		// not a failure, so it is not reported. Emitted outside m.mu for the
		// same reason as enqueue's emit: a must-deliver publish can block on
		// backpressure, and this file's lock discipline forbids blocking
		// Session calls under m.mu.
		//
		// Non-fatal: a failed turn does not end the session — the pump stays
		// live and the next queued prompt still runs.
		if err != nil && !errors.Is(err, context.Canceled) {
			m.sess.Emit(event.NewSessionError(m.id, err.Error(), false))
		}

		// Auto-compaction trigger: POST-FLIGHT, off the turn that just settled
		// cleanly. Deliberately not pre-flight (estimating the NEXT call's size
		// off the folded context before making it): gofer has no tokenizer, so a
		// pre-flight estimate would mean re-deriving byte-to-token math the SDK
		// doesn't provide, while a just-settled turn's Usage.InputTokens is a
		// REAL number — the provider already tokenized exactly this content to
		// answer the call (see [Session.LastUsage]'s doc). The cost is one turn
		// of lag: the turn that finally crosses the threshold runs at full size
		// before this compacts the NEXT one down. See config.Compaction's doc
		// for why 85% default headroom exists to absorb exactly that lag.
		//
		// Only after a clean, non-cancelled turn — a cancelled turn's usage may
		// be partial/absent, and there is nothing to react to.
		if err == nil {
			m.maybeAutoCompact()
			// A subagent's answer to the brief it was spawned with. AFTER the
			// compaction check on purpose: compaction is this session's own
			// bookkeeping and must not delay (or be delayed by) telling the
			// parent it is done.
			m.reportToParentOnce()
		}

		// turn.finished: cost and Updated changed even if the next loop
		// iteration immediately re-dispatches or goes idle.
		m.notify()
	}
}

// recoverFromContextOverflow reacts to a turn the provider REJECTED for
// exceeding the model's context window ([provider.ErrContextOverflow]): it
// announces the overflow, compacts, and re-issues the SAME turn text exactly
// once. overflowErr is what Prompt returned; the returned error replaces it
// in pump.
//
// Why this is a separate trigger from [maybeAutoCompact], not a replacement
// for it. That one reacts to SETTLED usage — a real measurement off a turn
// that actually ran — which is right for gradual growth and blind, by
// construction, to a single turn that overshoots the window in one step (a
// large read, a wide grep, a big bash output). The provider rejects the NEXT
// call outright, and a rejection generates nothing: no stream, no usage, so
// [Session.LastUsage] still reports the PREVIOUS turn's under-threshold
// figure and the threshold check never fires. The session wedges with no
// signal and no recovery but a manual /compact (jedwards1230/gofer#279). The
// two triggers are additive: only the threshold one can act BEFORE a
// rejection, and only this one can observe that a rejection happened at all.
//
// The reaction is the one [provider.ErrContextOverflow]'s doc prescribes —
// compact, then re-issue the same turn. The sentinel is matched with
// errors.Is and never by message text: each provider adapter normalizes its
// own vendor signal onto it, so the wording is explicitly not part of the
// contract (jedwards1230/agent-sdk-go#118).
//
// Deliberately NOT gated on [config.Compaction]'s Disabled/Threshold policy,
// which the threshold trigger alone consults. That policy answers "when
// should gofer compact AHEAD of trouble"; this path is the only way out of a
// session that is already wedged, so honouring an opt-out here would mean
// declining proactive compaction also declined ever recovering from the
// overflow that decision makes more likely. See config.Compaction.Disabled.
//
// Bounded at exactly one retry, and bounded STRUCTURALLY rather than by a
// counter: the retry is the single m.sess.Prompt call below, its result is
// returned straight to pump PAST the `if` that got here, and this function
// neither loops nor recurses. A second overflow therefore surfaces as an
// ordinary turn failure. That bound is the point — compacting again against a
// prompt that still does not fit is an infinite loop that burns tokens and
// looks, from the outside, exactly like a hang.
//
// The bound is one RETRY, not one compaction. A retry that SUCCEEDS settles
// like any other turn and so still reaches [maybeAutoCompact], which may
// compact a second time for the same queued prompt if the retried turn's
// measured usage crosses the threshold. That is correct — it is a real
// measurement off a turn that really ran, which is exactly what that trigger
// is for — and it cannot loop: maybeAutoCompact never re-dispatches.
func (m *managed) recoverFromContextOverflow(text string, overflowErr error, retireTurnCtx context.CancelFunc) error {
	// A FRESH turn context for the whole recovery: the original turnCtx is
	// spent (Prompt has returned), so reusing it would make every call below
	// an instant no-op. Published as m.turnCancel under mu so Interrupt/Kill
	// cancel the compaction round trip and the retry alike.
	//
	// ORDER IS LOAD-BEARING at both ends. Install the fresh cancel BEFORE
	// retiring the caller's, and clear to nil BEFORE cancelling on the way
	// out: m.turnCancel must never transiently name a context nobody is
	// running on. Interrupt reads it under mu and calls whatever it finds, so
	// a spent func there is a lie that returns nil while the session goes on
	// to run a whole summarizer round trip plus a retry turn the user asked
	// to stop. nil is honest by comparison — it means "idle, nothing to
	// interrupt", which is exactly true once the recovery has unwound. This
	// is a TOCTOU on a correctly-locked field rather than a data race, so
	// -race cannot see it either way.
	retryCtx, cancel := context.WithCancel(m.baseCtx)
	m.mu.Lock()
	m.turnCancel = cancel
	m.mu.Unlock()
	retireTurnCtx()
	defer func() {
		m.mu.Lock()
		m.turnCancel = nil
		m.mu.Unlock()
		cancel()
	}()

	// Announce BEFORE compacting, so the transcript reads notice →
	// session.compacted → the answer. Compaction is never silent in gofer, and
	// this case needs the notice more than the threshold-triggered one does:
	// the rejected call produced no output, so the user never saw the failure
	// that provoked it, and an unannounced compaction here reads as the
	// session skipping a beat. event is an SDK package and gofer consumes the
	// contract rather than extending it, so this reuses the existing
	// session.error kind — non-fatal, because the session is not ending.
	// Emitted outside mu, like every other Session call in this file.
	m.sess.Emit(event.NewSessionError(m.id,
		"context window exceeded — compacting the conversation and retrying this turn",
		false))

	if err := m.sess.Compact(retryCtx, ""); err != nil {
		if errors.Is(err, runner.ErrNothingToCompact) {
			// Nothing to shrink means the overshoot is a single oversized
			// payload in THIS turn rather than accumulated history, so a retry
			// would be rejected identically. The notice above promised a
			// remedy, so say plainly that it did not apply — otherwise the
			// transcript reads promise → raw rejection, and the user is left
			// to guess whether the compaction silently failed.
			m.sess.Emit(event.NewSessionError(m.id,
				"nothing to compact — this turn's own payload exceeds the context window, "+
					"so shortening the history cannot help; the turn was not retried",
				false))
			// Then surface the ORIGINAL rejection, not this one: "nothing to
			// compact" describes the remedy that did not apply, not the
			// problem the user actually has.
			return overflowErr
		}
		// Every other compaction failure surfaces carrying BOTH halves,
		// because the user needs both — the turn did not fit, AND the
		// automatic remedy could not run — and either alone is misleading.
		// See overflowRecoveryError for why that is a local type rather than
		// errors.Join.
		//
		// Classification survives the wrapper because stdlib errors.Is
		// traverses every branch of a multi-error Unwrap, and the overflow
		// sentinel is answered by the provider adapters' own Is(target)
		// methods (see provider/openai's APIError/StreamError) rather than by
		// ever being a value in the chain. Nothing in the SDK guarantees that
		// survival: its contract promises only that the sentinel propagates
		// UNWRAPPED through the loop and the runner, which is what keeps those
		// Is methods reachable for stdlib to find here.
		//
		// That traversal is also, USUALLY, what makes an interrupted recovery
		// fall out correctly: a cancelled Compact tends to contribute
		// context.Canceled, pump's emit filter sees it through the wrapper, and
		// an Esc stays silent as a cancelled turn should. Only "usually" —
		// Compact returns a bare ctx.Err() when cancelled before it starts, but
		// a cancellation mid-summarize surfaces through whatever the HTTP
		// adapter wrapped it in (a *url.Error unwrapping to context.Canceled,
		// in practice), which is adapter behavior and not an SDK-documented
		// guarantee. A missed suppression costs one extra session.error line,
		// never correctness.
		return &overflowRecoveryError{
			overflow: overflowErr,
			compact:  fmt.Errorf("compacting after context overflow: %w", err),
		}
	}

	// The retry is a fresh dispatch, so bump updated and notify exactly as pump
	// does for every other one — otherwise a slow recovery freezes the roster
	// row's age at the ORIGINAL dispatch and the session looks stalled while it
	// is in fact working. state is already stateRunning and stays there: the
	// session never went idle, so this is not a run-state transition.
	m.mu.Lock()
	m.updated = m.clock()
	m.mu.Unlock()
	m.notify()

	// Unlike pump's own dispatch this does NOT re-check m.closing under mu.
	// It does not need to: retryCtx derives from m.baseCtx, and stop() sets
	// closing and then cancels baseCtx, so a session that decides to stop
	// during the recovery kills this turn through the context instead. That
	// is the whole guarantee — a future edit that gives the retry a context
	// from anywhere else must reinstate the closing check.
	return m.sess.Prompt(retryCtx, text)
}

// overflowRecoveryError carries both halves of a failed overflow recovery —
// the provider's original rejection, and the compaction failure that stopped
// it being remedied — as one error that formats on a SINGLE LINE.
//
// It exists only because of that last word. [errors.Join] is otherwise exactly
// this type and would be the obvious spelling, but its Error() separates
// components with NEWLINES. This value reaches pump, which emits it as
// event.NewSessionError(m.id, err.Error(), false), and internal/render's
// Human.marker renders a session.error as a documented ONE-LINE row —
// "· <kind>  <detail>\n", with detail interpolated verbatim. A newline inside
// detail therefore splits that row in the non-TUI render paths (gofer demo and
// the JSONL renderer). Joining with "; " keeps the row intact and loses
// nothing a reader needs.
//
// So: do NOT "simplify" this back to errors.Join. The multi-line output is the
// bug it was written to fix.
type overflowRecoveryError struct {
	overflow error // the provider's context-window rejection
	compact  error // why the compaction that would have remedied it failed
}

// Error renders both halves on one line — see the type's doc for why that
// matters.
func (e *overflowRecoveryError) Error() string {
	return e.overflow.Error() + "; " + e.compact.Error()
}

// Unwrap returns BOTH errors, in the multi-error form Go 1.20+ errors.Is
// traverses. The single-error `Unwrap() error` form would silently drop a
// branch, and each branch is load-bearing: callers classify the overflow half
// with errors.Is(err, provider.ErrContextOverflow), and pump's own emit filter
// classifies the compact half with errors.Is(err, context.Canceled) to keep an
// interrupted recovery silent.
func (e *overflowRecoveryError) Unwrap() []error {
	return []error{e.overflow, e.compact}
}

// maybeAutoCompact checks the just-settled turn's measured usage against the
// LIVE compaction policy ([managed.compaction], re-read every call so a
// config edit takes effect on the very next turn) and fires
// [Session.Compact] when it crosses the threshold. Called only from pump,
// between turns — Compact's own documented precondition — never while
// turnCtx is active.
//
// A trigger failure (everything except [runner.ErrNothingToCompact], which
// just means the pressure check and Compact's own emptiness check
// disagreed — never observed in practice, since nothing else runs between
// them on this single-goroutine pump) is surfaced the same way a failed
// Prompt is: a session.error onto the session's own stream. Silently
// swallowing it would leave a session that keeps growing past its window
// with no visible explanation once the provider eventually rejects it.
func (m *managed) maybeAutoCompact() {
	policy := config.Compaction{}
	if m.compaction != nil {
		policy = m.compaction()
	}
	if !policy.AutoEnabled() {
		return
	}
	model, usage, ok := m.sess.LastUsage()
	if !ok {
		return
	}
	var contextWindow int
	if info, found := provider.Lookup(model); found {
		contextWindow = info.ContextWindow
	}
	if !shouldAutoCompact(usage, contextWindow, policy.Threshold()) {
		return
	}
	if err := m.sess.Compact(m.baseCtx, ""); err != nil {
		if errors.Is(err, runner.ErrNothingToCompact) {
			return
		}
		m.sess.Emit(event.NewSessionError(m.id, fmt.Sprintf("automatic compaction: %s", err.Error()), false))
	}
}

// reportDeadline bounds one child→parent report delivery. Generous, because the
// worker path's Report is a wire round trip to the router, and short enough that
// a session's teardown is never held open by an unreachable parent.
const reportDeadline = 30 * time.Second

// reportToParentOnce delivers this CHILD session's result to its parent, at
// most once for the session's whole life. Called only from pump, after a clean
// turn — a cancelled or failed turn produced no answer to report, and the
// session stays live to try again.
//
// # Why once, and not once per settled turn
//
// The report IS the answer to the brief the child was spawned with, and it
// arrives at the parent as a PROMPT. A parent that reacts to it by steering the
// child would get a second report, which it might react to again: two sessions
// prompting each other with no human in the loop is an unbounded loop that
// looks, from the outside, exactly like two agents working. Bounding it makes
// the fan-in per child exactly one message. A parent that wants more from a
// child steers it directly; that conversation has a human watching it.
//
// # The bound is DURABLE, not just in-memory
//
// The sync.Once alone would be wrong, and was: it belongs to this [managed],
// and a killed-then-resumed child is a NEW managed with a NEW Once, so `gofer
// resume <child-id>` (or the TUI's /resume) re-armed the report on every reopen
// and the parent got another copy per resume. The authoritative claim is the
// `reported` flag in the child's own sidecar (see [claimReport]); the Once is
// kept in front of it purely as the cheap in-process fast path, so a session
// that reports and then runs ten more turns pays one read-modify-write, not
// eleven.
//
// The claim is taken BEFORE delivery, so the failure direction is at-most-once:
// losing a report leaves a parent waiting, which a human notices, while
// duplicating one is the loop this bound exists to prevent.
//
// # Why the delivery is best-effort, but never silent
//
// A failed report is emitted as a non-fatal session.error on the CHILD's own
// stream rather than failing anything: the child's work is already done and
// journaled, so there is nothing left to fail. It must not be swallowed either
// — a parent waiting on a child that finished, reported into the void, and
// looks idle is the exact confusing state this whole path exists to prevent.
// The claim is consumed either way: a report that could not be delivered is not
// retried on the next turn, for the loop-bounding reason above.
func (m *managed) reportToParentOnce() {
	if m.reportParent == nil || m.parentID == "" {
		return
	}
	m.reportOnce.Do(func() {
		dir := filepath.Dir(m.sess.JournalPath())
		won, err := claimReport(dir, m.id)
		if err != nil {
			// The claim is what makes at-most-once true, so a claim that could
			// not be persisted must not be treated as won — reporting anyway
			// would re-arm on the next resume, which is the bug this replaced.
			// Loud rather than silent: the parent is waiting either way.
			m.sess.Emit(event.NewSessionError(m.id,
				fmt.Sprintf("could not record the subagent report claim, so no report was sent to parent session %s: %s",
					m.parentID, err.Error()), false))
			return
		}
		if !won {
			// A prior run of this session already reported (kill → resume →
			// another settled turn). Nothing to do, silently: this is the
			// bound working, not a failure.
			return
		}

		// WithoutCancel, plus a deadline of its own. m.baseCtx is cancelled by
		// Kill/Archive/Close, and the LAST report of a session's life is exactly
		// the one most likely to race a teardown — inheriting that cancellation
		// would fail the delivery with context.Canceled, emit a session.error
		// nobody is left watching, and burn the (already-persisted) claim. The
		// deadline is what keeps "uncancellable" from meaning "unbounded".
		ctx, cancel := context.WithTimeout(context.WithoutCancel(m.baseCtx), reportDeadline)
		defer cancel()

		text := formatSubagentReport(m.id, m.agent, lastAssistantText(m.sess.Fold()))
		if err := m.reportParent(ctx, m.parentID, text); err != nil {
			m.sess.Emit(event.NewSessionError(m.id,
				fmt.Sprintf("reporting to parent session %s: %s", m.parentID, err.Error()), false))
		}
	})
}

// shouldAutoCompact reports whether usage's measured token footprint has
// crossed threshold's fraction of contextWindow. contextWindow <= 0 (the
// model is unregistered/unknown) always returns false — an unknown window
// must never be treated as "full", since that would fire compaction off a
// guess rather than a measurement. The footprint is InputTokens +
// CacheReadTokens, the same formula [event.SessionCompacted]'s doc uses for
// the pre-compaction context size: exactly what the provider tokenized to
// answer the call, cache-served tokens included since they are just as much
// a part of the context that will need to fit again next turn.
func shouldAutoCompact(usage provider.Usage, contextWindow int, threshold float64) bool {
	if contextWindow <= 0 {
		return false
	}
	used := usage.InputTokens + usage.CacheReadTokens
	return float64(used) >= float64(contextWindow)*threshold
}

// watchPermissions maintains the live pending-approval count from the session's
// own event stream: +1 on a permission.requested, −1 on a permission.resolved.
// It runs for the session's whole lifetime, exiting when baseCtx is cancelled
// (stop) or the subscription closes (the session's broker shutting down),
// whichever comes first — so it never outlives the session. sub is closed on
// exit so the broker drops it.
func (m *managed) watchPermissions(sub *event.Subscription) {
	defer close(m.permDone)
	defer sub.Close()
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return
			}
			switch pe := e.(type) {
			case event.PermissionRequested:
				m.retainPerm(pe)
				m.adjustPending(1)
			case event.PermissionResolved:
				m.releasePerm(pe.ID)
				m.adjustPending(-1)
			}
		case <-m.baseCtx.Done():
			return
		}
	}
}

// decisionWatchBuffer sizes the decision subscription the standing watcher
// holds. A session has one outstanding decision at a time in practice, so this
// is pure headroom: the gate DROPS (and counts) rather than blocking when a
// subscriber's buffer fills, and a dropped update here would mean a request the
// host never relays — a turn blocked on a question no client is ever shown. The
// headroom is what makes that unreachable while the watcher is briefly busy
// inside a relay call.
//
// It is distinct from the CLIENT-side decision buffers (internal/tuibridge's
// decisionBuffer, internal/wirestream's decisionSubBuffer), which size
// subscriptions a client holds and can afford to be smaller: a client that
// misses a prompt still gets it replayed on its next attach. This one sizes the
// HOST's subscription to the gate itself — the one hop no replay covers, since
// the daemon's retained payload is written from this very update.
const decisionWatchBuffer = 64

// startDecisionWatch subscribes to this session's decision gate and starts the
// standing watcher that drives relay. It is idempotent — see decisionOnce — so
// [Supervisor.register] and [Supervisor.SetDecisionRelay] can both call it for
// the same session without racing two watchers onto one gate (which would
// double every relayed request).
//
// Subscribing here is also what satisfies [decision.Gate.Request]'s
// "is any client watching?" check under a host, so it must happen before the
// session's first turn can run — see [Supervisor.register].
func (m *managed) startDecisionWatch(relay DecisionRelay) {
	m.decisionOnce.Do(func() {
		m.decisionStarted.Store(true)
		go m.watchDecisions(m.decisions.Subscribe(decisionWatchBuffer), relay)
	})
}

// watchDecisions relays this session's structured-decision updates to the host
// (see [DecisionRelay]) for the session's whole lifetime.
//
// Unlike watchPermissions it does NOT exit on baseCtx cancellation, and that is
// the load-bearing difference: cancelling baseCtx is step one of teardown, and
// the resolutions that release the host's per-request bookkeeping (its route
// table, its retained replay payload, its outstanding client requests) are
// published by [decision.Gate.Close] in step three. A watcher that quit at step
// one would leave every open request of a killed session leaked in the host,
// and every attached client rendering a prompt nothing will ever resolve. So it
// exits only when the subscription closes — which Close guarantees, after it has
// published a resolution for every request still open.
//
// sub is closed on exit so the gate drops it.
func (m *managed) watchDecisions(sub *decision.Subscription, relay DecisionRelay) {
	defer close(m.decisionDone)
	defer sub.Close()
	for u := range sub.C {
		switch u.Kind {
		case decision.UpdateRequested:
			relay.RequestDecision(m.id, u.Request.ID, u.Request.Questions)
		case decision.UpdateResolved:
			relay.ResolveDecision(m.id, u.Request.ID)
		}
	}
}

// adjustPending bumps the outstanding-approval count by delta and pushes a
// fresh roster snapshot. It clamps at zero so a stray resolved (e.g. a
// replayed must-deliver event with no matching request) never drives the count
// negative. notify is called AFTER releasing m.mu, per the lock discipline in
// this file's doc.
func (m *managed) adjustPending(delta int) {
	m.mu.Lock()
	m.pending += delta
	if m.pending < 0 {
		m.pending = 0
	}
	m.mu.Unlock()
	m.notify()
}

// retainPerm records an outstanding permission request's full payload, so
// [Supervisor.ExplainPermission] can answer why that call was gated for as
// long as it IS outstanding. Called from watchPermissions beside the pending
// bump; no notify, because nothing on the roster snapshot changes.
func (m *managed) retainPerm(pe event.PermissionRequested) {
	m.mu.Lock()
	m.pendingPerms[pe.ID] = pe
	m.mu.Unlock()
}

// releasePerm drops a resolved request's retained payload. Idempotent — a
// stray resolved with no matching request (the same case adjustPending clamps
// at zero) simply deletes nothing.
func (m *managed) releasePerm(id string) {
	m.mu.Lock()
	delete(m.pendingPerms, id)
	m.mu.Unlock()
}

// pendingPerm returns the still-outstanding request with this call id, or
// ok=false once it has resolved (or if it never existed on this session).
func (m *managed) pendingPerm(id string) (event.PermissionRequested, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pe, ok := m.pendingPerms[id]
	return pe, ok
}

// stop marks m closing, cancels its base context (interrupting any in-flight
// turn, waking an idle pump, and waking watchPermissions), waits for both its
// pump and permission-watcher goroutines to exit, closes the decision gate,
// joins the decision watcher the close unwinds, and finally joins the
// OnRegister teardown (if any) — mirroring the permDone discipline above, so no
// observer goroutine outlives the session.
//
// The gate is closed here rather than left to the caller for the same reason
// the session's broker is closed on the way out: a client watching this
// session's decisions has a goroutine parked on its subscription's channel, and
// only the gate can end that stream. Closing it also clears any prompt still on
// a client's screen (each open request publishes its resolution first) and
// releases an ask_user call that somehow outlived the ctx cancel above. It is
// done AFTER the pump has exited so the ordering is unambiguous: the turn is
// already gone by the time the gate reports it closed.
func (m *managed) stop() {
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
	m.baseCancel()
	<-m.done
	<-m.permDone
	m.decisions.Close()
	// Joined AFTER the gate closes, because closing it is what ends the
	// watcher's subscription (see watchDecisions): waiting first would deadlock.
	// The started check keeps a relay-less supervisor — where no watcher was ever
	// spawned and decisionDone will never close — from blocking here. A
	// startDecisionWatch racing this teardown (SetDecisionRelay snapshotting the
	// roster just before a Kill takes the session out of it) is safe either way:
	// it either wins and is joined here, or it loses and subscribes to an already
	// closed gate, which hands back an already-closed subscription its watcher
	// returns from immediately.
	if m.decisionStarted.Load() {
		<-m.decisionDone
	}
	if m.teardown != nil {
		m.teardown()
	}
}

// snippet derives a one-line, bounded title from a prompt: the first non-empty
// line, with internal runs of whitespace collapsed to single spaces, trimmed,
// and truncated to maxTitle runes on a word boundary with an ellipsis when cut.
// A whitespace-only prompt yields "" (the caller treats that as "no title").
func snippet(s string) string {
	// First non-empty line: strings.Fields on the whole string would flatten a
	// multi-line prompt into its first line's worth plus the rest, so scan for
	// the first line with visible content first, then collapse within it.
	line := ""
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	// strings.Fields splits on any whitespace and drops empty fields, so the
	// join collapses internal whitespace to single spaces and trims the ends.
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return ""
	}

	const maxTitle = 60
	r := []rune(line)
	if len(r) <= maxTitle {
		return line
	}
	// Over budget: keep the first maxTitle runes, then avoid severing a word.
	// If the first dropped rune is a space, the cut already lands on a word
	// boundary; otherwise back off to the last space within the cut (or keep the
	// hard cut when the head is a single unbroken word). Fields collapsed
	// whitespace to single ASCII spaces, so IndexByte over the head is safe.
	head := string(r[:maxTitle])
	if r[maxTitle] != ' ' {
		if i := strings.LastIndexByte(head, ' '); i > 0 {
			head = head[:i]
		}
	}
	return strings.TrimRight(head, " ") + "…"
}
