package wirestream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// The gofer-native control/notification methods this reconstruction core reads
// off the wire, mirroring internal/daemon/handlers.go's methodGofer* constants
// (unexported there — cmd/gofer's ps/kill/archive commands already hardcode the
// same strings rather than import them, since they ARE the daemon's public wire
// contract, not an internal implementation detail).
const (
	// methodGoferRoster is the gofer/roster Call this core makes to enumerate
	// live sessions (see [Reconstructor.Roster] and [Reconstructor.sessionCwd]).
	methodGoferRoster = "gofer/roster"

	// methodGoferPermissionRequested / methodGoferPermissionResolved are the
	// gofer-native notifications the daemon fans a session's permission events
	// out to every attached peer with — mirroring
	// internal/daemon/handlers.go's own methodGoferPermissionRequested/
	// methodGoferPermissionResolved constants (unexported there; redeclared
	// here for the same reason as methodGoferRoster above). See reconstruct.go's
	// handlePermissionRequested/handlePermissionResolved.
	methodGoferPermissionRequested = "gofer/permission_requested"
	methodGoferPermissionResolved  = "gofer/permission_resolved"

	// methodGoferEvent is the M3 lossless-attach notification carrying a
	// source [event.Event]'s own MarshalJSON envelope, verbatim — mirroring
	// internal/daemon/handlers.go's own methodGoferEvent constant (unexported
	// there; redeclared here for the same reason as the others above). See
	// reconstruct.go's handleGoferEvent.
	methodGoferEvent = "gofer/event"
)

// subBuffer and replayDepth size each session's reconstructed [event.Broker]
// the same way the SDK's own session package sizes its live broker
// (session.defaultSubBuffer / defaultReplay): ample for one interactive
// turn's worth of deltas, with enough replay depth that a late Subscribe
// (peek/attach re-entering a session already in flight) still sees its
// lifecycle events.
const (
	subBuffer   = 256
	replayDepth = 256
)

// ErrClosed is returned by [Reconstructor.Subscribe]/[Reconstructor.SubscribeLive]
// once the Reconstructor has been closed — its brokers are reaped and the
// demuxer is gone, so no new subscription could ever receive events.
var ErrClosed = errors.New("wirestream: reconstructor is closed")

// turnEndChanBuffer bounds how many in-flight [Reconstructor.Send] calls can
// have their turn-end result queued for the demuxer at once before a sender
// would block. gofer's TUI drives at most one in-flight turn per session
// (the App only calls Send when a session is idle — see internal/tui/app.go's
// doSend), so a handful of concurrent sessions comfortably fits without ever
// blocking a Send goroutine on delivery.
const turnEndChanBuffer = 16

// loadChanBuffer bounds how many in-flight [Reconstructor.loadHistory] calls
// can have their completion queued for the demuxer at once before that
// goroutine would block sending — sized the same as turnEndChanBuffer and for
// the same reason: a handful of sessions attaching for the first time at once
// comfortably fits.
const loadChanBuffer = 16

// Reconstructor drains a [*daemon.Client]'s inbound notification stream and
// reconstructs each session's typed [event.Event] stream from it into a
// per-session [*event.Broker] — the tui-free core behind
// [github.com/jedwards1230/gofer/internal/daemonbridge]. It owns one background
// demuxer goroutine (started by [New]) for the lifetime of the Reconstructor;
// see reconstruct.go.
type Reconstructor struct {
	client *daemon.Client

	mu       sync.Mutex
	sessions map[string]*sessionState

	turnEndCh chan turnEnd
	loadCh    chan *sessionState
	closed    chan struct{}
	wg        sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// New returns a Reconstructor driving the daemon reached through client. The
// caller dials client (see [daemon.Dial]) and hands it over; New starts the
// demuxer goroutine that drains [daemon.Client.Notifications] for the
// lifetime of the Reconstructor. Call [Reconstructor.Close] to tear both down.
func New(client *daemon.Client) *Reconstructor {
	r := &Reconstructor{
		client:    client,
		sessions:  make(map[string]*sessionState),
		turnEndCh: make(chan turnEnd, turnEndChanBuffer),
		loadCh:    make(chan *sessionState, loadChanBuffer),
		closed:    make(chan struct{}),
	}
	r.wg.Add(1)
	go r.demux()
	return r
}

// Close shuts the underlying client connection down, waits for the demuxer
// goroutine to exit (guaranteed once the connection closes — see demux), and
// closes every session's reconstructed broker so any live subscription's
// channel observes a clean close rather than hanging forever. Idempotent —
// a second call is a no-op returning the first call's result (the underlying
// [daemon.Client.Close] is idempotent too).
func (r *Reconstructor) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		r.closeErr = r.client.Close()
		r.wg.Wait()
	})
	return r.closeErr
}

// sessionState is one session's replay state plus its reconstructed event
// broker. The broker is safe for concurrent use on its own (see
// [event.Broker]); turnTerminated is mutated ONLY by the demuxer goroutine
// (see reconstruct.go's handleGoferEvent/handleTurnEnd) — no additional
// locking is needed for it, since that goroutine is the sole writer and
// reader.
type sessionState struct {
	id     string
	broker *event.Broker

	// turnTerminated reports whether a terminal gofer/event turn.finished
	// (stop reason != "tool_use") has already been replayed for the
	// currently-open turn — see handleGoferEvent. handleTurnEnd reads it to
	// decide whether Send's Call outcome still needs a FALLBACK terminal
	// event published (the ordinary case does not: the real one already
	// arrived via gofer/event).
	turnTerminated bool

	// loadDone gates history-before-live ordering: it is closed either
	// immediately (RegisterFresh, for a session the CALLER just created via
	// session/new — which carries no history by construction) or once a
	// triggered session/load's replay has been fully applied to broker
	// (finishLoad, in reconstruct.go). [Reconstructor.Send] waits on it before
	// publishing or dispatching anything for a session, so a live turn can
	// never race a still-settling history replay onto the broker ahead of it
	// — see loadHistory's doc in reconstruct.go for the full argument.
	loadDone chan struct{}
}

// newSessionState returns id's zero-value reconstruction record: an empty
// broker and an open (not yet closed) loadDone. Both of session's/
// RegisterFresh's creation paths build one; they differ only in whether they
// leave loadDone open (session, pending a triggered load) or close it right
// away (RegisterFresh, a session known to have no history).
func newSessionState(id string) *sessionState {
	return &sessionState{
		id:       id,
		broker:   event.NewBroker(event.WithReplay(replayDepth)),
		loadDone: make(chan struct{}),
	}
}

// session returns id's reconstruction state, creating it on first reference
// from any of Subscribe, SubscribeLive, Send, or the demuxer. Guarded by mu
// since it is called from arbitrary caller goroutines (consumer ops) as well
// as the single demuxer goroutine.
//
// Creating a brand-new entry here — as opposed to finding one the caller
// already pre-registered via [Reconstructor.RegisterFresh] — is this core's ONE
// trigger for a session/load-driven history replay: it starts
// [Reconstructor.loadHistory] on a dedicated goroutine (never inline on this
// method's own caller, and especially never inline on the demuxer — see
// loadHistory's doc for why) before returning. Because the map insert happens
// under mu before that goroutine is started, and every other caller of
// session for the same id will find the map entry already present and return
// early, loadHistory is started at most once per session id.
func (r *Reconstructor) session(id string) *sessionState {
	r.mu.Lock()
	if rec, ok := r.sessions[id]; ok {
		r.mu.Unlock()
		return rec
	}
	// After Close (r.closed is closed and closeAllBrokers has reaped the map),
	// never create a fresh broker: nothing would ever close it or publish to
	// it, so a subscription on it would hang forever and the broker would leak.
	// A nil return signals "closed" to callers. mu serializes this with
	// closeAllBrokers, so a broker created just before Close is still reaped.
	select {
	case <-r.closed:
		r.mu.Unlock()
		return nil
	default:
	}
	rec := newSessionState(id)
	r.sessions[id] = rec
	r.mu.Unlock()

	go r.loadHistory(rec)
	return rec
}

// RegisterFresh pre-registers id's reconstruction state with loadDone already
// closed, for a session the CALLER just created via session/new: that response
// carries no history by construction, so there is nothing to load. Calling it
// before the caller's own follow-up Subscribe (and, when the create carried a
// first prompt, [Reconstructor.Send]) guarantees they find the entry already
// mapped and never trigger a history load — see session's doc. It is a no-op
// if the session is already registered, or if the Reconstructor is closed
// (nothing would ever publish to or close a fresh broker), matching session's
// own nil-on-closed contract.
func (r *Reconstructor) RegisterFresh(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[id]; ok {
		return
	}
	select {
	case <-r.closed:
		return
	default:
	}
	rec := newSessionState(id)
	close(rec.loadDone)
	r.sessions[id] = rec
}

// SessionInfo is the client-side mirror of internal/daemon/wire.go's
// unexported sessionInfoDTO — the wire shape of a gofer/roster row. It is
// redeclared here (rather than imported, since the daemon's type is
// unexported by design: it IS the wire contract, not an internal detail any
// client should reach into) with matching json tags. Consumers that need a
// domain-shaped row (e.g. the TUI's tui.SessionInfo) map from this; see
// internal/daemonbridge's toTUISessionInfo.
type SessionInfo struct {
	ID      string         `json:"id"`
	Title   string         `json:"title"`
	Status  string         `json:"status"`
	Model   string         `json:"model"`
	Cost    provider.Cost  `json:"cost"`
	Usage   provider.Usage `json:"usage"`
	Queued  int            `json:"queued"`
	Created time.Time      `json:"created"`
	Updated time.Time      `json:"updated"`
	Project string         `json:"project"`
	Live    bool           `json:"live"`
	// Cwd, like the rest of this DTO, mirrors internal/daemon/wire.go's field
	// of the same name — used internally by [Reconstructor.sessionCwd] to drive
	// session/load's required cwd (see loadHistory), and surfaced by consumers
	// (internal/daemonbridge's toTUISessionInfo) as their row's cwd group key.
	Cwd string `json:"cwd"`
	// Pending is the session's live outstanding-permission-request count —
	// contract #2 of the M3 approvals-relay work: the daemon side
	// (internal/daemon/wire.go) encodes [supervisor.SessionInfo.Pending] as
	// "pending,omitempty". Additive field: an older daemon simply never sends
	// it, and this decodes to the zero value (no badge), matching M2's
	// always-0 behavior.
	Pending int `json:"pending,omitempty"`
	// BinaryVersion mirrors internal/daemon/wire.go's field of the same name:
	// the gofer build version of the process running the session. Under M6
	// process isolation a router stamps it from the owning WORKER's gofer/hello
	// handshake, so a roster can show mixed binary versions across a daemon
	// upgrade. Additive and live-only: an older daemon (or any offline row)
	// simply never sends it and this decodes to "".
	BinaryVersion string `json:"binaryVersion,omitempty"`
}

// Roster calls gofer/roster and decodes the raw wire rows. Consumers map the
// result to their own domain row type (internal/daemonbridge's Roster maps to
// tui.SessionInfo); [Reconstructor.sessionCwd] reuses it for the Cwd field a
// mapped row may not carry.
func (r *Reconstructor) Roster(ctx context.Context) ([]SessionInfo, error) {
	raw, err := r.client.Call(ctx, methodGoferRoster, nil)
	if err != nil {
		return nil, fmt.Errorf("wirestream: roster: %w", err)
	}
	var dtos []SessionInfo
	if err := json.Unmarshal(raw, &dtos); err != nil {
		return nil, fmt.Errorf("wirestream: decode %s response: %w", methodGoferRoster, err)
	}
	return dtos, nil
}

// sessionCwd looks up sessionID's persisted working directory from the live
// roster (the same gofer/roster wire call [Reconstructor.Roster] makes), for
// [Reconstructor.loadHistory] to pass as session/load's required cwd. It
// returns "" if the session isn't (yet, or ever) in the roster or the call
// itself fails — not a guess at a real path, but exactly the fallback the
// daemon's own resolveSessionCwd already applies to an empty session/load
// cwd (its own working directory, see internal/daemon/handlers.go), so it is
// a value the daemon is guaranteed to accept.
func (r *Reconstructor) sessionCwd(ctx context.Context, sessionID string) string {
	dtos, err := r.Roster(ctx)
	if err != nil {
		return ""
	}
	for _, d := range dtos {
		if d.ID == sessionID {
			return d.Cwd
		}
	}
	return ""
}

// Subscribe returns the reconstructed event stream for sessionID WITH backlog
// replay: the session broker's retained must-deliver events (see
// [event.WithReplay]) are replayed to this late subscriber first, so peek/attach
// re-entering a session already in flight still sees its lifecycle events and
// any still-open permission request. Creates the session's reconstruction state
// (and broker) on first reference if this is the first
// Subscribe/SubscribeLive/Send/notification the core has seen for it.
func (r *Reconstructor) Subscribe(_ context.Context, sessionID string) (*event.Subscription, error) {
	rec := r.session(sessionID)
	if rec == nil {
		return nil, ErrClosed
	}
	return rec.broker.Subscribe(event.FilterAll, subBuffer), nil
}

// SubscribeLive returns the reconstructed event stream for sessionID WITHOUT
// backlog replay — [event.Broker.SubscribeLive], the no-replay counterpart of
// [Reconstructor.Subscribe]: the subscription observes only events published
// from the point of subscription forward, with none of the retained
// must-deliver backlog [event.WithReplay] keeps. This is the stream a consumer
// wants when it has already sourced any needed history another way (the M6
// router's SubscribeLive fan-out) and must not re-emit the replay backlog.
//
// IMPORTANT — do NOT first-reference a session through SubscribeLive. Like
// Subscribe, it creates the session's reconstruction state on first reference,
// and that first reference is [session]'s ONE trigger for a session/load
// history replay (unless the id was pre-registered via [RegisterFresh]). That
// replay publishes the session's whole history onto the broker AFTER this
// subscription already exists, so it arrives as live events this no-replay
// subscription DOES observe — defeating the no-replay intent and flooding the
// "live" stream with history, racily. SubscribeLive only yields a clean
// live-only stream for a session that is ALREADY referenced: call
// [RegisterFresh] first for a session this core just created via session/new
// (the router's fresh-spawn path), or let a prior [Subscribe]'s history load
// settle first for an adopted/attached one. TestSubscribeLiveFirstReferenceReplaysHistory
// (external) pins this actual behavior.
func (r *Reconstructor) SubscribeLive(_ context.Context, sessionID string) (*event.Subscription, error) {
	rec := r.session(sessionID)
	if rec == nil {
		return nil, ErrClosed
	}
	return rec.broker.SubscribeLive(event.FilterAll, subBuffer), nil
}

// Load references sessionID and blocks until its one-shot history load (the
// session/load replay) has fully settled onto the reconstructed broker —
// history plus any retained must-deliver backlog the source re-emits on attach,
// chiefly an OPEN [event.PermissionRequested] for a turn blocked mid-approval
// (docs/milestones/M6-process-isolation.md §7). It is the safe adoption entry
// point: the M6 router calls Load FIRST — so history and any still-open
// permission re-surface into the broker (retained by [event.WithReplay]) — and
// only THEN [Reconstructor.SubscribeLive] for the live stream. That ordering
// satisfies the reference-before-SubscribeLive contract (see SubscribeLive's
// doc) WITHOUT relying on [Reconstructor.Subscribe]'s first-reference replay
// side effect: a subsequent Subscribe replays whatever Load settled, a
// subsequent SubscribeLive sees only new events.
//
// Mechanically Load reuses the exact history-load path Subscribe/SubscribeLive
// trigger on first reference: [Reconstructor.session] creates the session's
// state and starts [Reconstructor.loadHistory] (issuing session/load) at most
// once per id; Load then waits on rec.loadDone, which the demuxer closes only
// after it has drained and applied every notification that load replayed (see
// loadHistory's ordering proof). For an already-referenced or RegisterFresh'd
// session, loadDone is already closed and Load returns as soon as it observes
// it. It returns ctx.Err() if ctx is cancelled before the load settles, or
// [ErrClosed] if the Reconstructor is (or becomes) closed.
func (r *Reconstructor) Load(ctx context.Context, sessionID string) error {
	rec := r.session(sessionID)
	if rec == nil {
		return ErrClosed
	}
	select {
	case <-rec.loadDone:
		return nil
	case <-r.closed:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}
