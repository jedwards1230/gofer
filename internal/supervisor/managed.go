package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"

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
	sess      Session
	id        string
	project   string
	model     string
	cwd       string
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
}

// newManaged builds a managed session ready to register: idle, empty queue,
// its own cancellable base context. If onRegister is non-nil, it is invoked
// here — with the session, before m is returned to register for roster
// publish — and its returned teardown (if any) is stashed on m for stop to
// join later. Calling it here, rather than after publish, closes the race
// where a concurrent Kill/Archive could otherwise observe a live session
// with no teardown stashed yet (see Config.OnRegister's doc).
func newManaged(sess Session, model, effort string, now time.Time, clock func() time.Time, notify func(), cwd string, gate *loop.Gate, decisions *decision.Gate, onRegister func(sess Session) (stop func())) *managed {
	ctx, cancel := context.WithCancel(context.Background())
	m := &managed{
		sess:         sess,
		id:           sess.ID(),
		project:      filepath.Base(filepath.Dir(sess.JournalPath())),
		model:        model,
		effort:       effort,
		cwd:          cwd,
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

	m.mu.Lock()
	defer m.mu.Unlock()

	status := StatusNeedsInput
	if m.state == stateRunning || len(m.queue) > 0 {
		status = StatusWorking
	}
	title := m.title
	if title == "" {
		title = m.project
	}
	return SessionInfo{
		ID:          m.id,
		Title:       title,
		Status:      status,
		Model:       m.model,
		Effort:      m.effort,
		Cost:        report.Cost,
		Usage:       report.Usage,
		Pending:     m.pending,
		Created:     m.createdAt,
		Updated:     m.updated,
		Project:     m.project,
		JournalPath: m.sess.JournalPath(),
		Queued:      len(m.queue),
		Live:        true,
		Cwd:         m.cwd,
	}
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

		// turn.finished: cost and Updated changed even if the next loop
		// iteration immediately re-dispatches or goes idle.
		m.notify()
	}
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
			switch e.(type) {
			case event.PermissionRequested:
				m.adjustPending(1)
			case event.PermissionResolved:
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
