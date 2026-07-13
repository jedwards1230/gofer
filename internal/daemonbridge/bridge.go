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

// sessionState is one session's reconstruction state plus its
// reconstructed event broker. The broker is safe for concurrent use on its
// own (see [event.Broker]); the open-message fields (hasOpen/openKind/text)
// are mutated ONLY by the demuxer goroutine (see reconstruct.go) — no
// additional locking is needed for them, since that goroutine is the sole
// writer.
type sessionState struct {
	id     string
	broker *event.Broker

	hasOpen  bool
	openKind event.MessageKind
	text     string
}

// session returns id's reconstruction state, creating it (with a fresh
// broker) on first reference from any of Subscribe, Send, or the demuxer.
// Guarded by mu since it is called from arbitrary caller goroutines (TUI ops)
// as well as the single demuxer goroutine.
func (s *Supervisor) session(id string) *sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.sessions[id]; ok {
		return rec
	}
	// After Close (s.closed is closed and closeAllBrokers has reaped the map),
	// never create a fresh broker: nothing would ever close it or publish to
	// it, so a subscription on it would hang forever and the broker would leak.
	// A nil return signals "closed" to callers. mu serializes this with
	// closeAllBrokers, so a broker created just before Close is still reaped.
	select {
	case <-s.closed:
		return nil
	default:
	}
	rec := &sessionState{
		id:     id,
		broker: event.NewBroker(event.WithReplay(replayDepth)),
	}
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
// Summary/Pending/Artifacts have no wire representation in M2 (see
// sessionInfoDTO's doc and internal/daemon/wire.go) and are left at their
// zero values.
func toTUISessionInfo(d sessionInfoDTO) tui.SessionInfo {
	return tui.SessionInfo{
		ID:      d.ID,
		Title:   d.Title,
		Status:  statusFromWire(d.Status),
		Model:   d.Model,
		Cost:    d.Cost,
		Usage:   d.Usage,
		Created: d.Created,
		Updated: d.Updated,
	}
}

// Roster calls gofer/roster and maps the result to the TUI's row type.
func (s *Supervisor) Roster(ctx context.Context) ([]tui.SessionInfo, error) {
	raw, err := s.client.Call(ctx, methodGoferRoster, nil)
	if err != nil {
		return nil, fmt.Errorf("daemonbridge: roster: %w", err)
	}
	var dtos []sessionInfoDTO
	if err := json.Unmarshal(raw, &dtos); err != nil {
		return nil, fmt.Errorf("daemonbridge: decode %s response: %w", methodGoferRoster, err)
	}
	out := make([]tui.SessionInfo, len(dtos))
	for i, d := range dtos {
		out[i] = toTUISessionInfo(d)
	}
	return out, nil
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
func (s *Supervisor) Create(ctx context.Context, prompt string, opts tui.CreateOptions) (tui.SessionInfo, error) {
	raw, err := s.client.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: opts.Cwd})
	if err != nil {
		return tui.SessionInfo{}, fmt.Errorf("daemonbridge: create: %w", err)
	}
	var resp acp.NewSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return tui.SessionInfo{}, fmt.Errorf("daemonbridge: decode %s response: %w", acp.MethodSessionNew, err)
	}

	now := time.Now()
	info := tui.SessionInfo{
		ID:      resp.SessionID,
		Model:   opts.Model,
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
