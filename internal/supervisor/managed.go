package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
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
	// permDone is closed by the watchPermissions goroutine when it returns, so
	// stop joins it alongside the pump — leaving no subscription goroutine
	// behind on shutdown.
	permDone chan struct{}
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
	// only (see [Supervisor.LastError]). Provider/loop errors already reach
	// subscribers as session.error events on the session's own stream, and a
	// cancelled turn is expected — so the pump never treats a Prompt error
	// as a supervisor-level failure.
	lastErr error
}

// newManaged builds a managed session ready to register: idle, empty queue,
// its own cancellable base context. If onRegister is non-nil, it is invoked
// here — with the session, before m is returned to register for roster
// publish — and its returned teardown (if any) is stashed on m for stop to
// join later. Calling it here, rather than after publish, closes the race
// where a concurrent Kill/Archive could otherwise observe a live session
// with no teardown stashed yet (see Config.OnRegister's doc).
func newManaged(sess Session, model string, now time.Time, clock func() time.Time, notify func(), cwd string, gate *loop.Gate, onRegister func(sess Session) (stop func())) *managed {
	ctx, cancel := context.WithCancel(context.Background())
	m := &managed{
		sess:       sess,
		id:         sess.ID(),
		project:    filepath.Base(filepath.Dir(sess.JournalPath())),
		model:      model,
		cwd:        cwd,
		createdAt:  now,
		updated:    now,
		clock:      clock,
		notify:     notify,
		baseCtx:    ctx,
		baseCancel: cancel,
		done:       make(chan struct{}),
		gate:       gate,
		permDone:   make(chan struct{}),
		submitCh:   make(chan struct{}, 1),
		state:      stateIdle,
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
// session title from the first prompt. It returns ErrNotLive if the session
// has already begun closing.
func (m *managed) enqueue(text string) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return ErrNotLive
	}
	if m.title == "" {
		m.title = snippet(text)
	}
	m.queue = append(m.queue, text)
	m.mu.Unlock()

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
// pump and permission-watcher goroutines to exit, and finally joins the
// OnRegister teardown (if any) — mirroring the permDone discipline above, so
// no observer goroutine outlives the session.
func (m *managed) stop() {
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
	m.baseCancel()
	<-m.done
	<-m.permDone
	if m.teardown != nil {
		m.teardown()
	}
}

// snippet renders a one-line, bounded title from a prompt: leading/trailing
// space trimmed, truncated at the first newline, and capped at maxTitle runes.
func snippet(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const maxTitle = 80
	if r := []rune(s); len(r) > maxTitle {
		return strings.TrimSpace(string(r[:maxTitle]))
	}
	return s
}
