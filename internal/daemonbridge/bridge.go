package daemonbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/tui"
)

// The gofer-native control methods this bridge drives, mirroring
// internal/daemon/handlers.go's methodGofer* constants (unexported there —
// cmd/gofer's ps/kill/archive commands already hardcode the same strings
// rather than import them, since they ARE the daemon's public wire contract,
// not an internal implementation detail).
const (
	methodGoferRoster  = "gofer/roster"
	methodGoferKill    = "gofer/kill"
	methodGoferArchive = "gofer/archive"

	// methodGoferPermissionRequested / methodGoferPermissionResolved are the
	// gofer-native notifications the daemon fans a session's permission events
	// out to every attached peer with — mirroring
	// internal/daemon/handlers.go's own methodGoferPermissionRequested/
	// methodGoferPermissionResolved constants (unexported there; redeclared
	// here for the same reason as methodGoferRoster et al. above). See
	// reconstruct.go's handlePermissionRequested/handlePermissionResolved.
	methodGoferPermissionRequested = "gofer/permission_requested"
	methodGoferPermissionResolved  = "gofer/permission_resolved"

	// methodGoferEvent is the M3 lossless-attach notification carrying a
	// source [event.Event]'s own MarshalJSON envelope, verbatim — mirroring
	// internal/daemon/handlers.go's own methodGoferEvent constant (unexported
	// there; redeclared here for the same reason as the others above). See
	// reconstruct.go's handleGoferEvent.
	methodGoferEvent = "gofer/event"
)

// methodPermissionReply is the JSON-RPC method literal the daemon exposes to
// answer a pending permission request — contract #1 of the M3 approvals-relay
// work: it is a bare notification (no id, no response), decoded daemon-side
// into an [event.PermissionReply] and routed to the session's
// loop.Gate.Reply. See [Supervisor.Reply].
const methodPermissionReply = "permission.reply"

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

// errClosed is returned by [Supervisor.Subscribe] once the supervisor has been
// closed — its brokers are reaped and the demuxer is gone, so no new
// subscription could ever receive events.
var errClosed = errors.New("daemonbridge: supervisor is closed")

// turnEndChanBuffer bounds how many in-flight [Supervisor.Send] calls can
// have their turn-end result queued for the demuxer at once before a sender
// would block. gofer's TUI drives at most one in-flight turn per session
// (the App only calls Send when a session is idle — see internal/tui/app.go's
// doSend), so a handful of concurrent sessions comfortably fits without ever
// blocking a Send goroutine on delivery.
const turnEndChanBuffer = 16

// loadChanBuffer bounds how many in-flight [Supervisor.loadHistory] calls can
// have their completion queued for the demuxer at once before that goroutine
// would block sending — sized the same as turnEndChanBuffer and for the same
// reason: a handful of sessions attaching for the first time at once
// comfortably fits.
const loadChanBuffer = 16

// Supervisor is a [tui.Supervisor] backed by a running `gofer daemon`,
// reached over a [*daemon.Client] connection. It owns one background demuxer
// goroutine (started by [New]) that drains the client's inbound notification
// stream and reconstructs each session's [event.Event] stream from it — see
// reconstruct.go.
type Supervisor struct {
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

// Supervisor satisfies the TUI's consumer interface. Failing this assertion
// means the daemon's wire contract drifted from what the TUI drives.
var _ tui.Supervisor = (*Supervisor)(nil)

// New returns a Supervisor driving the daemon reached through client. The
// caller dials client (see [daemon.Dial]) and hands it over; New starts the
// demuxer goroutine that drains [daemon.Client.Notifications] for the
// lifetime of the Supervisor. Call [Supervisor.Close] to tear both down.
func New(client *daemon.Client) *Supervisor {
	s := &Supervisor{
		client:    client,
		sessions:  make(map[string]*sessionState),
		turnEndCh: make(chan turnEnd, turnEndChanBuffer),
		loadCh:    make(chan *sessionState, loadChanBuffer),
		closed:    make(chan struct{}),
	}
	s.wg.Add(1)
	go s.demux()
	return s
}

// Close shuts the underlying client connection down, waits for the demuxer
// goroutine to exit (guaranteed once the connection closes — see demux), and
// closes every session's reconstructed broker so any live subscription's
// channel observes a clean close rather than hanging forever. Idempotent —
// a second call is a no-op returning the first call's result (the underlying
// [daemon.Client.Close] is idempotent too).
func (s *Supervisor) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.closeErr = s.client.Close()
		s.wg.Wait()
	})
	return s.closeErr
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
	// immediately (registerFresh, for a session THIS bridge just created via
	// Create — a session/new response carries no history by construction) or
	// once a triggered session/load's replay has been fully applied to broker
	// (finishLoad, in reconstruct.go). [Supervisor.Send] waits on it before
	// publishing or dispatching anything for a session, so a live turn can
	// never race a still-settling history replay onto the broker ahead of it
	// — see loadHistory's doc in reconstruct.go for the full argument.
	loadDone chan struct{}
}

// newSessionState returns id's zero-value reconstruction record: an empty
// broker and an open (not yet closed) loadDone. Both of session's/
// registerFresh's creation paths build one; they differ only in whether they
// leave loadDone open (session, pending a triggered load) or close it right
// away (registerFresh, a session known to have no history).
func newSessionState(id string) *sessionState {
	return &sessionState{
		id:       id,
		broker:   event.NewBroker(event.WithReplay(replayDepth)),
		loadDone: make(chan struct{}),
	}
}

// session returns id's reconstruction state, creating it on first reference
// from any of Subscribe, Send, or the demuxer. Guarded by mu since it is
// called from arbitrary caller goroutines (TUI ops) as well as the single
// demuxer goroutine.
//
// Creating a brand-new entry here — as opposed to finding one Create already
// pre-registered via [Supervisor.registerFresh] — is this bridge's ONE
// trigger for a session/load-driven history replay: it starts
// [Supervisor.loadHistory] on a dedicated goroutine (never inline on this
// method's own caller, and especially never inline on the demuxer — see
// loadHistory's doc for why) before returning. Because the map insert happens
// under mu before that goroutine is started, and every other caller of
// session for the same id will find the map entry already present and return
// early, loadHistory is started at most once per session id.
func (s *Supervisor) session(id string) *sessionState {
	s.mu.Lock()
	if rec, ok := s.sessions[id]; ok {
		s.mu.Unlock()
		return rec
	}
	// After Close (s.closed is closed and closeAllBrokers has reaped the map),
	// never create a fresh broker: nothing would ever close it or publish to
	// it, so a subscription on it would hang forever and the broker would leak.
	// A nil return signals "closed" to callers. mu serializes this with
	// closeAllBrokers, so a broker created just before Close is still reaped.
	select {
	case <-s.closed:
		s.mu.Unlock()
		return nil
	default:
	}
	rec := newSessionState(id)
	s.sessions[id] = rec
	s.mu.Unlock()

	go s.loadHistory(rec)
	return rec
}

// registerFresh pre-registers id's reconstruction state with loadDone
// already closed, for a session THIS Supervisor just created via
// [Supervisor.Create]: a session/new response carries no history by
// construction, so there is nothing to load. Calling it before Create
// returns id to its caller guarantees the Subscribe (and, when Create was
// given a first prompt, the Send) that follows finds the entry already
// mapped and never triggers a history load — see session's doc. A nil
// return (supervisor closed) is a safe no-op for the caller, matching
// session's own nil-on-closed contract.
func (s *Supervisor) registerFresh(id string) *sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.sessions[id]; ok {
		return rec
	}
	select {
	case <-s.closed:
		return nil
	default:
	}
	rec := newSessionState(id)
	close(rec.loadDone)
	s.sessions[id] = rec
	return rec
}

// sessionInfoDTO is the client-side mirror of internal/daemon/wire.go's
// unexported sessionInfoDTO — the wire shape of a gofer/roster row. It is
// redeclared here (rather than imported, since the daemon's type is
// unexported by design: it IS the wire contract, not an internal detail any
// client should reach into) with matching json tags.
type sessionInfoDTO struct {
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
	// of the same name — used internally by [Supervisor.sessionCwd] to drive
	// session/load's required cwd (see loadHistory), and surfaced through
	// [toTUISessionInfo] as [tui.SessionInfo.Cwd], the roster's cwd group key.
	Cwd string `json:"cwd"`
	// Pending is the session's live outstanding-permission-request count —
	// contract #2 of the M3 approvals-relay work: the daemon side
	// (internal/daemon/wire.go) encodes [supervisor.SessionInfo.Pending] as
	// "pending,omitempty". Additive field: an older daemon simply never sends
	// it, and this decodes to the zero value (no badge), matching M2's
	// always-0 behavior.
	Pending int `json:"pending,omitempty"`
}

// statusFromWire maps the daemon's roster Status string — literally
// [supervisor.SessionStatus.String]'s output ("working", "needs-input",
// "finished", or "unknown" for a future/unrecognized value) — to the TUI's
// own [tui.SessionStatus] enum. This is an explicit string switch, not an
// ordinal cast: the wire carries the string precisely so the two enums can
// drift independently (see internal/daemon/wire.go's toSessionInfoDTO).
// An unrecognized value falls back to StatusNeedsInput rather than the
// zero-value StatusWorking, so a wire/enum drift never makes a session look
// like it has a turn in flight when it does not.
func statusFromWire(s string) tui.SessionStatus {
	switch s {
	case "working":
		return tui.StatusWorking
	case "needs-input":
		return tui.StatusNeedsInput
	case "finished":
		return tui.StatusFinished
	default:
		return tui.StatusNeedsInput
	}
}

// toTUISessionInfo maps one wire roster row to the TUI's row type.
// Summary/Artifacts have no wire representation yet (see sessionInfoDTO's
// doc and internal/daemon/wire.go) and are left at their zero values; Pending
// is live as of the M3 approvals-relay work (contract #2).
func toTUISessionInfo(d sessionInfoDTO) tui.SessionInfo {
	return tui.SessionInfo{
		ID:      d.ID,
		Title:   d.Title,
		Status:  statusFromWire(d.Status),
		Model:   d.Model,
		Cwd:     d.Cwd,
		Cost:    d.Cost,
		Usage:   d.Usage,
		Pending: d.Pending,
		Created: d.Created,
		Updated: d.Updated,
	}
}

// rosterDTOs calls gofer/roster and decodes the raw wire rows, shared by
// [Supervisor.Roster] (which maps them to the TUI's row type) and
// [Supervisor.sessionCwd] (which needs the Cwd field Roster's mapped
// tui.SessionInfo doesn't carry).
func (s *Supervisor) rosterDTOs(ctx context.Context) ([]sessionInfoDTO, error) {
	raw, err := s.client.Call(ctx, methodGoferRoster, nil)
	if err != nil {
		return nil, fmt.Errorf("daemonbridge: roster: %w", err)
	}
	var dtos []sessionInfoDTO
	if err := json.Unmarshal(raw, &dtos); err != nil {
		return nil, fmt.Errorf("daemonbridge: decode %s response: %w", methodGoferRoster, err)
	}
	return dtos, nil
}

// Roster calls gofer/roster and maps the result to the TUI's row type.
func (s *Supervisor) Roster(ctx context.Context) ([]tui.SessionInfo, error) {
	dtos, err := s.rosterDTOs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tui.SessionInfo, len(dtos))
	for i, d := range dtos {
		out[i] = toTUISessionInfo(d)
	}
	return out, nil
}

// sessionCwd looks up sessionID's persisted working directory from the live
// roster (the same gofer/roster wire call [Supervisor.Roster] makes), for
// [Supervisor.loadHistory] to pass as session/load's required cwd. It
// returns "" if the session isn't (yet, or ever) in the roster or the call
// itself fails — not a guess at a real path, but exactly the fallback the
// daemon's own resolveSessionCwd already applies to an empty session/load
// cwd (its own working directory, see internal/daemon/handlers.go), so it is
// a value the daemon is guaranteed to accept.
func (s *Supervisor) sessionCwd(ctx context.Context, sessionID string) string {
	dtos, err := s.rosterDTOs(ctx)
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

// Subscribe returns the reconstructed event stream for sessionID, creating
// its reconstruction state (and broker) on first reference if this is the
// first Subscribe/Send/notification the bridge has seen for it.
func (s *Supervisor) Subscribe(_ context.Context, sessionID string) (*event.Subscription, error) {
	rec := s.session(sessionID)
	if rec == nil {
		return nil, errClosed
	}
	return rec.broker.Subscribe(event.FilterAll, subBuffer), nil
}

// Create starts a new session via session/new. The daemon resolves its own
// default model (session/new carries no model field in ACP; see
// internal/daemon's handleSessionNew) — opts.Model is display-only here,
// carried onto the returned row but never sent to the daemon. When prompt is
// non-empty, Create kicks off [Supervisor.Send] in the background (the same
// fire-and-forget path a subsequent Send call would take) and returns a
// minimal row immediately; the App's 1s roster poll refreshes it with the
// daemon's authoritative state.
//
// Create pre-registers the new session's reconstruction state via
// [Supervisor.registerFresh] as soon as it has an id — before optionally
// calling Send, and well before the TUI's own follow-up Subscribe can
// possibly reach this Supervisor (see app.go's createdMsg handling: it
// switchSession/Subscribes only after Create's tea.Cmd returns) — so neither
// ever triggers a needless session/load for a session that, by construction,
// has no history yet.
func (s *Supervisor) Create(ctx context.Context, prompt string, opts tui.CreateOptions) (tui.SessionInfo, error) {
	raw, err := s.client.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: opts.Cwd})
	if err != nil {
		return tui.SessionInfo{}, fmt.Errorf("daemonbridge: create: %w", err)
	}
	var resp acp.NewSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return tui.SessionInfo{}, fmt.Errorf("daemonbridge: decode %s response: %w", acp.MethodSessionNew, err)
	}
	s.registerFresh(resp.SessionID)

	now := time.Now()
	info := tui.SessionInfo{
		ID:      resp.SessionID,
		Model:   opts.Model,
		Cwd:     opts.Cwd,
		Status:  tui.StatusNeedsInput,
		Created: now,
		Updated: now,
	}
	if prompt != "" {
		info.Status = tui.StatusWorking
		if err := s.Send(ctx, resp.SessionID, prompt); err != nil {
			return info, err
		}
	}
	return info, nil
}

// Kill calls gofer/kill.
func (s *Supervisor) Kill(ctx context.Context, sessionID string) error {
	if _, err := s.client.Call(ctx, methodGoferKill, map[string]string{"sessionId": sessionID}); err != nil {
		return fmt.Errorf("daemonbridge: kill %s: %w", sessionID, err)
	}
	return nil
}

// Archive calls gofer/archive.
func (s *Supervisor) Archive(ctx context.Context, sessionID string) error {
	if _, err := s.client.Call(ctx, methodGoferArchive, map[string]string{"sessionId": sessionID}); err != nil {
		return fmt.Errorf("daemonbridge: archive %s: %w", sessionID, err)
	}
	return nil
}

// Interrupt sends session/cancel, per ACP a notification with no response —
// the in-flight session/prompt Call (see [Supervisor.Send]) resolves on its
// own once the daemon observes the cancellation, publishing the resulting
// TurnFinished(stop=cancelled) through the normal reconstruction path.
func (s *Supervisor) Interrupt(ctx context.Context, sessionID string) error {
	if err := s.client.Notify(ctx, acp.MethodSessionCancel, acp.CancelNotification{SessionID: sessionID}); err != nil {
		return fmt.Errorf("daemonbridge: interrupt %s: %w", sessionID, err)
	}
	return nil
}

// Reply answers a pending permission request by sending [methodPermissionReply]
// — a bare notification, matching the "permission.reply" op's own
// fire-and-forget contract (see event.PermissionReply's doc: it carries no
// response). sessionID is not part of the wire payload: the daemon resolves
// a request by id alone (see [Supervisor.session]'s reconstruction — the
// same id [event.PermissionRequested]/[event.PermissionResolved] already
// carry), matching [tui.Supervisor.Reply]'s doc.
func (s *Supervisor) Reply(ctx context.Context, sessionID, id string, allow, remember bool) error {
	verdict := event.VerdictDeny
	if allow {
		verdict = event.VerdictAllow
	}
	params := struct {
		ID       string        `json:"id"`
		Verdict  event.Verdict `json:"verdict"`
		Remember bool          `json:"remember,omitempty"`
	}{ID: id, Verdict: verdict, Remember: remember}
	if err := s.client.Notify(ctx, methodPermissionReply, params); err != nil {
		return fmt.Errorf("daemonbridge: reply %s (session %s): %w", id, sessionID, err)
	}
	return nil
}
