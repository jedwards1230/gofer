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
	"github.com/jedwards1230/gofer/internal/decision"
)

// The gofer-native control/notification methods this reconstruction core reads
// off the wire, mirroring internal/daemon/handlers.go's methodGofer* constants
// (unexported there — cmd/gofer's ps/kill/archive commands already hardcode the
// same strings rather than import them, since they ARE the daemon's public wire
// contract, not an internal implementation detail).
const (
	// methodGoferRoster is the gofer/roster Call this core makes to enumerate
	// live sessions (see [Reconstructor.Roster]).
	methodGoferRoster = "gofer/roster"

	// methodGoferOverview is the gofer/overview Call this core makes to enumerate
	// the OVERVIEW roster — every non-archived session, live plus offline, so the
	// overview survives a daemon restart (see [Reconstructor.OverviewRoster]).
	// Distinct from methodGoferRoster's live-only set.
	methodGoferOverview = "gofer/overview"

	// methodGoferPermissionRequested / methodGoferPermissionResolved are the
	// gofer-native notifications the daemon fans a session's permission events
	// out to every attached peer with — mirroring
	// internal/daemon/handlers.go's own methodGoferPermissionRequested/
	// methodGoferPermissionResolved constants (unexported there; redeclared
	// here for the same reason as methodGoferRoster above). See reconstruct.go's
	// handlePermissionRequested/handlePermissionResolved.
	methodGoferPermissionRequested = "gofer/permission_requested"
	methodGoferPermissionResolved  = "gofer/permission_resolved"

	// methodGoferDecisionRequested / methodGoferDecisionResolved are the
	// gofer-native notifications the daemon fans a session's structured-decision
	// (ask_user) requests out to every attached peer with — mirroring
	// internal/daemon/handlers.go's constants of the same names (unexported
	// there; redeclared here for the same reason as the others above). Unlike the
	// permission pair they do NOT reconstruct into events — the SDK's Event union
	// carries no decision kind, which is the whole reason internal/decision
	// exists — so they land on a per-session [decision.Stream] instead of the
	// session broker. See reconstruct.go's handleDecisionRequested/Resolved.
	methodGoferDecisionRequested = "gofer/decision_requested"
	methodGoferDecisionResolved  = "gofer/decision_resolved"

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

// decisionSubBuffer sizes each decision subscription this core hands out. A
// session has one outstanding decision at a time in practice, so this is pure
// headroom for a consumer that is mid-frame when one arrives; delivery is
// drop-on-full (see decision.Subscribe), so the cost of an undersized buffer
// would be a missed prompt rather than a stalled demuxer. It matches the
// in-process path's own buffer (internal/tuibridge's decisionBuffer) in intent,
// and is larger only because this stream also carries a replay burst on attach.
const decisionSubBuffer = 32

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

// EventSink observes every gofer/event this core reconstructs, called on the
// demuxer goroutine immediately BEFORE the event is published to its session's
// broker (see reconstruct.go's handleGoferEvent). It receives BOTH halves of
// the same frame:
//
//   - raw: the notification's params exactly as they arrived — the source
//     [event.Event]'s own MarshalJSON envelope, VERBATIM. A consumer forwarding
//     a session's stream onwards (the M6 router re-fanning a worker's events to
//     its own clients) writes these bytes through unchanged, so the frame a
//     client receives is byte-identical to the one the worker emitted: no
//     decode/re-encode step exists to drop a field ACP's lossy session/update
//     projection would (tool.call.finished's Diagnostics/Spill*, tool.call.delta
//     fragments). raw is owned by the caller for the duration of the call only —
//     a sink that retains it past return must copy it.
//   - ev: the concrete event this core just decoded from raw. Handed over so a
//     consumer that ALSO needs a decoded event (an ACP session/update
//     projection) gets it for free rather than decoding the same bytes twice.
//
// Both fan-outs a consumer drives from one sink call therefore run on ONE
// goroutine, in wire order, marshaling nothing.
//
// # Accepted design risk (documented, not fixed)
//
// The sink runs ON the demuxer goroutine, so whatever it does — including a
// client WebSocket write — is synchronous with this session's reconstruction: a
// wedged client stalls it. That blast radius is bounded to ONE session, because
// one worker is one connection is one Reconstructor (M6 §3), which is the
// isolation property working as intended.
//
// Within that one session, though, the radius is WIDER than its event stream:
// it is the session's whole CONTROL PLANE. This goroutine is also the sole
// drainer of the connection's [daemon.Client] notification channel, so while it
// is stalled in a sink the client's readLoop blocks on that full channel and
// every Call over the connection hangs with it — gofer/roster, gofer/kill,
// gofer/archive, session/prompt. A stalled sink does not merely silence the
// session; it makes the session unkillable over its own socket. Sinks must
// therefore bound whatever they do here: the daemon's relays do, via
// [github.com/jedwards1230/gofer/internal/daemon]'s relayWriteTimeout.
//
// A hand-off channel would decouple them at the cost of the single-goroutine
// wire-ordering guarantee this core's whole replay argument rests on (see
// reconstruct.go's package-level ordering proof), and is deliberately NOT used.
type EventSink func(sessionID string, raw json.RawMessage, ev event.Event)

// Option configures a [Reconstructor] at construction. Options exist because
// [New] starts the demuxer goroutine before it returns: anything the demuxer
// reads must be installed BEFORE that, so there is no post-hoc setter for it to
// race with.
type Option func(*Reconstructor)

// WithEventSink installs sink, invoked for every reconstructed gofer/event (see
// [EventSink]). A nil sink is ignored. It is deliberately a construction-time
// option and NOT a setter: the demuxer goroutine [New] starts reads this field
// on every event, so a mutable field would be a data race by construction.
func WithEventSink(sink EventSink) Option {
	return func(r *Reconstructor) { r.sink = sink }
}

// CwdMissingSink observes the ONE session/load failure this core routes onward
// rather than dropping: the daemon rejecting a load because the session's
// RECORDED working directory no longer exists (the daemon's session-cwd-missing
// code, read back with [daemon.SessionCwdMissing]). cwd is that recorded
// directory — what the consumer names when it asks the user where to reopen the
// session instead.
//
// It is deliberately NARROW. [Reconstructor.loadHistory] still discards every
// OTHER load error exactly as before (jedwards1230/gofer#325 — a failed load
// leaves the session starting from whatever live events arrive next), because
// this one failure is the only one a consumer can act on: it has a remedy the
// user chooses, not a message the user reads.
//
// Unlike [EventSink] it does NOT run on the demuxer goroutine — it runs on the
// per-session loadHistory goroutine, so it cannot stall the connection's control
// plane. It must still return promptly: it runs before that goroutine hands the
// session to the demuxer to settle, and a [Reconstructor.Send] waiting on the
// load waits behind it. Post a message and return; never block on a UI.
type CwdMissingSink func(sessionID, cwd string)

// WithCwdMissingSink installs sink, invoked when a session/load this core issued
// was rejected because the session's recorded cwd is gone (see
// [CwdMissingSink]). A nil sink is ignored. Construction-time for the same
// reason as [WithEventSink]: [New] starts the demuxer — and with it the loads it
// triggers — before it returns, so a post-hoc setter would race them.
func WithCwdMissingSink(sink CwdMissingSink) Option {
	return func(r *Reconstructor) { r.cwdMissing = sink }
}

// Reconstructor drains a [*daemon.Client]'s inbound notification stream and
// reconstructs each session's typed [event.Event] stream from it into a
// per-session [*event.Broker] — the tui-free core behind
// [github.com/jedwards1230/gofer/internal/daemonbridge]. It owns one background
// demuxer goroutine (started by [New]) for the lifetime of the Reconstructor;
// see reconstruct.go.
type Reconstructor struct {
	client *daemon.Client

	// sink, when non-nil, observes every reconstructed gofer/event (see
	// [EventSink]). Written once by [New] before the demuxer starts and never
	// mutated afterwards, so the demuxer's reads need no synchronization.
	sink EventSink

	// cwdMissing, when non-nil, observes a session/load rejected because the
	// session's recorded cwd is gone (see [CwdMissingSink]). Written once by
	// [New] before any load can be triggered and never mutated afterwards, so
	// the loadHistory goroutines' reads need no synchronization.
	cwdMissing CwdMissingSink

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
//
// Options ([WithEventSink]) are applied BEFORE the demuxer starts, which is the
// only safe point: once it is running, anything it reads is shared with it.
func New(client *daemon.Client, opts ...Option) *Reconstructor {
	r := &Reconstructor{
		client:    client,
		sessions:  make(map[string]*sessionState),
		turnEndCh: make(chan turnEnd, turnEndChanBuffer),
		loadCh:    make(chan *sessionState, loadChanBuffer),
		closed:    make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
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

	// decisions is this session's client-side structured-decision stream: the
	// parallel, non-event channel the daemon's gofer/decision_* notifications
	// reconstruct onto (see reconstruct.go's handleDecisionRequested). It is
	// separate from broker because a decision is NOT an [event.Event] — the SDK's
	// union is closed and has no decision kind — so there is nothing to publish
	// onto a broker even in principle. Safe for concurrent use on its own, like
	// the broker beside it.
	decisions *decision.Stream

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

	// loadOnce guards loadDone's close, because a session can now be loaded
	// more than once (see reload): a second load settling would otherwise
	// close an already-closed channel and panic. The gate is one-way by
	// design — once history has settled it stays settled — so the retry
	// re-issues the RPC without reopening it.
	loadOnce sync.Once

	// reload arms ONE further history load on the next CONSUMER-facing
	// reference to this session — [Reconstructor.reference], never the plain
	// [Reconstructor.session] lookup the demuxer makes per frame. It is set only
	// by the cwd-missing branch of loadHistory: that load did not merely fail,
	// it raised a signal whose whole point is that the user answers it, and
	// memoizing it would make the second attempt — pressing Enter on the same
	// roster row again — a silent empty attach instead of the prompt
	// (jedwards1230/gofer#326). Cleared without loading by
	// [Reconstructor.RegisterFresh], whose caller is issuing its own load.
	// Guarded by [Reconstructor.mu], like the sessions map it lives in.
	reload bool
}

// newSessionState returns id's zero-value reconstruction record: an empty
// broker and an open (not yet closed) loadDone. Both of session's/
// RegisterFresh's creation paths build one; they differ only in whether they
// leave loadDone open (session, pending a triggered load) or close it right
// away (RegisterFresh, a session known to have no history).
func newSessionState(id string) *sessionState {
	return &sessionState{
		id:        id,
		broker:    event.NewBroker(event.WithReplay(replayDepth)),
		decisions: decision.NewStream(),
		loadDone:  make(chan struct{}),
	}
}

// settleLoad closes loadDone, at most once. Both settling paths go through it —
// RegisterFresh's immediate close and finishLoad's post-drain one — so the
// retried load a cwd-missing signal arms (see sessionState.reload) settles onto
// an already-closed gate instead of panicking on a double close.
func (rec *sessionState) settleLoad() {
	rec.loadOnce.Do(func() { close(rec.loadDone) })
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
//
// This is the REFERENCE-NEUTRAL entry: it never re-loads an already-mapped
// session, whoever asks. That is what makes it safe on the demuxer's own hot
// path — handleGoferEvent calls it for every inbound event frame — and the
// cwd-missing retry deliberately does NOT live here for exactly that reason
// (see [Reconstructor.reference]).
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
	if r.isClosed() {
		r.mu.Unlock()
		return nil
	}
	rec := newSessionState(id)
	r.sessions[id] = rec
	r.mu.Unlock()

	go r.loadHistory(rec)
	return rec
}

// reference returns id's reconstruction state for a CONSUMER-facing reference —
// [Reconstructor.Subscribe], [Reconstructor.SubscribeLive] and
// [Reconstructor.Load], the three calls that mean "a consumer is attaching to
// this session". It is [Reconstructor.session] plus the cwd-missing retry.
//
// # The cwd-missing retry
//
// A load rejected because the session's RECORDED directory is gone arms
// rec.reload (loadHistory), and this method consumes that arming to issue the
// load again. Without it the memo would make the remedy one-shot: the user
// answers the prompt that failure raised with "cancel", presses Enter on the
// same roster row again, and the second attach finds the map entry, issues no
// session/load, raises no signal and shows an empty attach screen saying
// nothing — the pre-fix silence, back again on the second try
// (jedwards1230/gofer#326).
//
// # Why the retry lives HERE and not in session()
//
// Because "a consumer is attaching" is the only reference kind a retry may
// answer. session() is also called for every inbound event frame
// (handleGoferEvent), for every turn end, and by Send — and the demuxer's
// per-frame call is the dangerous one: an explicit re-init (the prompt's own
// remedy, [daemonbridge.Supervisor.Resume] with the user's directory) replays
// the whole history back as gofer/event frames BEFORE its response, so the
// first of those frames would consume the arming and fire a second, blank-cwd
// session/load — which now succeeds, because the session is live by then, and
// replays the entire history onto the same broker AGAIN. That is precisely the
// double-render internal/tui's resumeSession is written to avoid, and it would
// have fired on every successful re-init.
//
// The supersede rule in [Reconstructor.RegisterFresh] closes the other half:
// after an explicit resume, the consumer's follow-up Subscribe must not consume
// a now-stale arming either.
//
// The retry re-issues the RPC and nothing else: the session's broker, decision
// stream and loadDone gate are the SAME ones, still in the map, so
// [Reconstructor.Close] still reaps that broker (a broker dropped from the map
// would leave any subscription on it hanging forever) and loadDone — closed by
// the first, failed load — stays closed rather than reopening a gate a
// [Reconstructor.Send] might already have passed. Only the cwd-missing branch
// arms it, so every other load failure stays memoized exactly as before
// (jedwards1230/gofer#325 untouched), and each retry must be armed afresh by a
// further failure, so this can never spin.
func (r *Reconstructor) reference(id string) *sessionState {
	rec := r.session(id)
	if rec == nil {
		return nil
	}
	if r.takeReload(rec) {
		go r.loadHistory(rec)
	}
	return rec
}

// takeReload consumes rec's pending cwd-missing retry, reporting whether one was
// armed. Always clears the flag, so a retry is issued at most once per arming
// however many consumers reference the session.
func (r *Reconstructor) takeReload(rec *sessionState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	armed := rec.reload && !r.isClosed()
	rec.reload = false
	return armed
}

// RegisterFresh pre-registers id's reconstruction state with loadDone already
// closed, for a session the CALLER just created via session/new: that response
// carries no history by construction, so there is nothing to load. Calling it
// before the caller's own follow-up Subscribe (and, when the create carried a
// first prompt, [Reconstructor.Send]) guarantees they find the entry already
// mapped and never trigger a history load — see session's doc. It is a no-op
// if the Reconstructor is closed (nothing would ever publish to or close a
// fresh broker), matching session's own nil-on-closed contract.
//
// For an ALREADY-registered id it is not quite a no-op: it clears any pending
// cwd-missing retry ([sessionState.reload]). The caller is declaring that IT is
// issuing this session's load — [daemonbridge.Supervisor.Resume] calls this
// immediately before its own session/load — which supersedes the retry the
// core would otherwise issue. Without the clear, the consumer's follow-up
// Subscribe after a successful re-init would consume the stale arming and load
// the session a second time, replaying its whole history onto the broker twice.
func (r *Reconstructor) RegisterFresh(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.sessions[id]; ok {
		rec.reload = false
		return
	}
	if r.isClosed() {
		return
	}
	rec := newSessionState(id)
	rec.settleLoad()
	r.sessions[id] = rec
}

// isClosed reports whether [Reconstructor.Close] has been called. It takes no
// lock of its own (the channel is closed exactly once and never reopened);
// callers holding mu use it to serialize their decision with closeAllBrokers.
func (r *Reconstructor) isClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
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
	// of the same name — the directory the session runs in, surfaced by
	// consumers (internal/daemonbridge's toTUISessionInfo) as their row's cwd
	// group key. This core deliberately does NOT feed it back to the daemon as
	// session/load's cwd (see loadHistory): echoing a value read out of the
	// journal would be indistinguishable, server-side, from a directory the user
	// explicitly chose.
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
	// Effort mirrors internal/daemon/wire.go's field of the same name: the
	// session's reasoning effort ("", "low", "medium", "high"). Additive and
	// omitempty on both sides — an older daemon never sends it and this decodes
	// to "", which is also what an unset level looks like, so a consumer needs
	// no version check to read it.
	Effort string `json:"effort,omitempty"`
	// ParentID, Agent and Depth mirror internal/daemon/wire.go's fields of the
	// same names: the session's subagent link (which session spawned it, which
	// agent identity it runs as, its depth in the tree). Additive — a daemon
	// predating subagents never sends them, and an ordinary root session on a
	// current daemon omits all three, so they decode to the zero values that
	// mean "a root session".
	ParentID string `json:"parentId,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Depth    int    `json:"depth,omitempty"`
	// LastUsage mirrors internal/daemon/wire.go's field of the same name: the
	// most recently completed turn's token usage in the session's current
	// folded context — the measured proxy for how full the context window is
	// right now, distinct from the ACCUMULATED Usage above. Additive: an
	// older daemon never sends it and this decodes to the zero value, which
	// also correctly describes a session with no settled turn yet.
	LastUsage provider.Usage `json:"lastUsage"`
	// ContextWindow mirrors internal/daemon/wire.go's field of the same name:
	// the active model's context-window size in tokens, resolved server-side
	// — 0 when unknown. Additive and omitempty: an older daemon never sends
	// it and 0 is also the correct "unknown" value.
	ContextWindow int `json:"contextWindow,omitempty"`
}

// Roster calls gofer/roster and decodes the raw wire rows. Consumers map the
// result to their own domain row type (internal/daemonbridge's Roster maps to
// tui.SessionInfo).
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

// OverviewRoster calls gofer/overview and decodes the raw wire rows — the
// overview's roster source: every NON-archived session, live rows overlaid with
// their live snapshot and offline rows (Live=false) rebuilt from their journals,
// so the overview survives a daemon restart. It is Roster's persistent twin
// (gofer/roster is live-only); offline rows carry Live=false, which the same DTO
// already round-trips (gofer/ps sends them today), so the decode is unchanged.
func (r *Reconstructor) OverviewRoster(ctx context.Context) ([]SessionInfo, error) {
	raw, err := r.client.Call(ctx, methodGoferOverview, nil)
	if err != nil {
		return nil, fmt.Errorf("wirestream: overview roster: %w", err)
	}
	var dtos []SessionInfo
	if err := json.Unmarshal(raw, &dtos); err != nil {
		return nil, fmt.Errorf("wirestream: decode %s response: %w", methodGoferOverview, err)
	}
	return dtos, nil
}

// Subscribe returns the reconstructed event stream for sessionID WITH backlog
// replay: the session broker's retained must-deliver events (see
// [event.WithReplay]) are replayed to this late subscriber first, so peek/attach
// re-entering a session already in flight still sees its lifecycle events and
// any still-open permission request. Creates the session's reconstruction state
// (and broker) on first reference if this is the first
// Subscribe/SubscribeLive/Send/notification the core has seen for it.
func (r *Reconstructor) Subscribe(_ context.Context, sessionID string) (*event.Subscription, error) {
	rec := r.reference(sessionID)
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
	rec := r.reference(sessionID)
	if rec == nil {
		return nil, ErrClosed
	}
	return rec.broker.SubscribeLive(event.FilterAll, subBuffer), nil
}

// Decisions returns a subscription to sessionID's reconstructed
// structured-decision stream: the questions an agent on the far side asked with
// the ask_user tool and is blocked on, as reconstructed from the daemon's
// gofer/decision_requested / gofer/decision_resolved notifications. Every
// request already open is replayed first (see [decision.Stream.Subscribe]), so
// a consumer attaching mid-question still sees the prompt.
//
// It is a SECOND stream alongside [Reconstructor.Subscribe], not part of the
// event stream, for the same reason it is on the in-process path: the SDK's
// Event union carries no decision kind (see internal/decision's package doc).
//
// Like Subscribe it creates the session's reconstruction state on first
// reference — which triggers the one-shot history load, whose session/load is
// ALSO what makes the daemon replay any open decision (see internal/daemon's
// handleSessionLoad), so a first-reference Decisions is self-sufficient: it does
// not depend on a prior Subscribe to surface an already-open question.
//
// The caller owns the returned subscription and must Close it. It does not have
// to poll for the connection ending: [Reconstructor.Close] closes every
// session's stream, so the channel closing is how a consumer learns the stream
// is over — exactly as with an event subscription.
//
// It references through [Reconstructor.session], NOT [Reconstructor.reference]:
// it does not consume a pending cwd-missing retry. That is deliberate and not an
// oversight — a consumer arms both streams for ONE attach (the TUI subscribes,
// then subscribes decisions off that subscription's readiness), so consuming
// here as well would issue a second load per attach, since the retry's own
// failure re-arms the flag before this call runs. One attach, one retry. When
// Decisions genuinely IS the first reference, session() starts the load anyway.
func (r *Reconstructor) Decisions(_ context.Context, sessionID string) (*decision.Subscription, error) {
	rec := r.session(sessionID)
	if rec == nil {
		return nil, ErrClosed
	}
	return rec.decisions.Subscribe(decisionSubBuffer), nil
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
	rec := r.reference(sessionID)
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
