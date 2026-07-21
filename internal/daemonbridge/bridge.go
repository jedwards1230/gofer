package daemonbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/wirestream"
)

// The gofer-native control methods this bridge drives directly (the rest go
// through the reconstruction core — see [wirestream]), mirroring
// internal/daemon/handlers.go's methodGofer* constants (unexported there —
// cmd/gofer's ps/kill/archive commands already hardcode the same strings
// rather than import them, since they ARE the daemon's public wire contract,
// not an internal implementation detail).
const (
	methodGoferKill      = "gofer/kill"
	methodGoferArchive   = "gofer/archive"
	methodGoferSetModel  = "gofer/set_model"
	methodGoferSetEffort = "gofer/set_effort"
)

// methodPermissionReply is the JSON-RPC method literal the daemon exposes to
// answer a pending permission request — contract #1 of the M3 approvals-relay
// work: it is a bare notification (no id, no response), decoded daemon-side
// into an [event.PermissionReply] and routed to the session's
// loop.Gate.Reply. See [Supervisor.Reply].
const methodPermissionReply = "permission.reply"

// sessionInfoDTO is the wire shape of a gofer/roster row. It now lives on the
// reconstruction core as [wirestream.SessionInfo] (the one wire decoder shared
// with the M6 router); this alias keeps the tui-shaped translation below
// ([statusFromWire]/[toTUISessionInfo]) reading against a local name.
type sessionInfoDTO = wirestream.SessionInfo

// Supervisor is a [tui.Supervisor] backed by a running `gofer daemon`,
// reached over a [*daemon.Client] connection. It composes a tui-free
// [*wirestream.Reconstructor] — which owns the background demuxer draining the
// client's notification stream and reconstructing each session's [event.Event]
// stream (see the wirestream package) — and layers the TUI-shaped translation
// on top: mapping the daemon's wire roster rows to [tui.SessionInfo] and
// exposing the create/kill/archive/set-model/interrupt/reply control surface
// [tui.Model] drives.
//
// # Why this is not a thin pass-through
//
// [internal/tuibridge.Adapter] wraps an in-process [*supervisor.Supervisor]:
// the TUI and the supervisor share memory, so Subscribe is a direct pass
// through to the supervisor's own [*event.Broker]. daemonbridge has no such
// shared memory — the daemon's supervisor runs in a different process (or a
// different machine entirely) and exposes only the wire. So the reconstruction
// core RECONSTRUCTS the typed [event.Event] stream [tui.Model.Ingest] expects
// from that narrower wire projection; this Supervisor is the thin TUI-facing
// adapter over it.
type Supervisor struct {
	// core reconstructs each session's typed event stream from the daemon's
	// wire and owns the demuxer goroutine + the client's lifecycle (Close).
	core *wirestream.Reconstructor
	// client is the SAME connection core drives; this bridge also issues the
	// handful of direct control Calls that need no reconstruction (create,
	// kill, archive, set-model, interrupt, reply). [daemon.Client] is safe for
	// concurrent Call/Notify, so sharing it with the core's demuxer is sound.
	client *daemon.Client
}

// Supervisor satisfies the TUI's consumer interface. Failing this assertion
// means the daemon's wire contract drifted from what the TUI drives.
var _ tui.Supervisor = (*Supervisor)(nil)

// New returns a Supervisor driving the daemon reached through client. The
// caller dials client (see [daemon.Dial]) and hands it over; New builds the
// reconstruction core, which starts the demuxer goroutine that drains
// [daemon.Client.Notifications] for the lifetime of the Supervisor. Call
// [Supervisor.Close] to tear both down.
func New(client *daemon.Client) *Supervisor {
	return &Supervisor{
		core:   wirestream.New(client),
		client: client,
	}
}

// Close tears down the reconstruction core: it shuts the underlying client
// connection down, waits for the demuxer goroutine to exit, and closes every
// session's reconstructed broker so any live subscription observes a clean
// close. Idempotent (see [wirestream.Reconstructor.Close]).
func (s *Supervisor) Close() error {
	return s.core.Close()
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
// Summary/Artifacts have no wire representation yet (see
// [wirestream.SessionInfo]'s doc and internal/daemon/wire.go) and are left at
// their zero values; Pending is live as of the M3 approvals-relay work
// (contract #2).
func toTUISessionInfo(d sessionInfoDTO) tui.SessionInfo {
	return tui.SessionInfo{
		ID:            d.ID,
		Title:         d.Title,
		Status:        statusFromWire(d.Status),
		Model:         d.Model,
		Effort:        d.Effort,
		Cwd:           d.Cwd,
		Cost:          d.Cost,
		Usage:         d.Usage,
		Pending:       d.Pending,
		BinaryVersion: d.BinaryVersion,
		Created:       d.Created,
		Updated:       d.Updated,
	}
}

// Roster calls gofer/roster (via the reconstruction core's one wire decoder)
// and maps the result to the TUI's row type.
func (s *Supervisor) Roster(ctx context.Context) ([]tui.SessionInfo, error) {
	dtos, err := s.core.Roster(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tui.SessionInfo, len(dtos))
	for i, d := range dtos {
		out[i] = toTUISessionInfo(d)
	}
	return out, nil
}

// Subscribe returns the reconstructed event stream for sessionID, creating its
// reconstruction state (and broker) on first reference if this is the first
// Subscribe/Send/notification the bridge has seen for it. It uses the core's
// replaying subscribe (see [wirestream.Reconstructor.Subscribe]) so a
// peek/attach re-entering a session already in flight still sees its lifecycle
// events and any open permission request.
func (s *Supervisor) Subscribe(ctx context.Context, sessionID string) (*event.Subscription, error) {
	return s.core.Subscribe(ctx, sessionID)
}

// Send submits prompt as sessionID's next turn, delegating to the
// reconstruction core (see [wirestream.Reconstructor.Send]): fire-and-forget,
// history-before-live ordered, one-outstanding-turn-per-session caller-enforced.
func (s *Supervisor) Send(ctx context.Context, sessionID, prompt string) error {
	return s.core.Send(ctx, sessionID, prompt)
}

// Create starts a new session via session/new. opts.Model, when non-empty, is
// forwarded on the request and the daemon honors it for the new session (see
// internal/daemon's handleSessionNew); an empty opts.Model resolves to the
// daemon's own configured default. Either way the returned row carries the
// model the daemon ASSIGNED, read off the response's `_meta` (see
// [newSessionResponse]), so the roster shows what the session actually runs
// before the next poll confirms it.
//
// It used to carry opts.Model — the REQUEST — instead, which on the normal path
// (no model sent, daemon picks its default) is the empty string, so the row
// could never show the real model (issue #162). When prompt is non-empty, Create kicks
// off [Supervisor.Send] in the background (the same fire-and-forget path a
// subsequent Send call would take) and returns a minimal row immediately; the
// App's 1s roster poll refreshes it with the daemon's authoritative state.
//
// Create pre-registers the new session's reconstruction state via
// [wirestream.Reconstructor.RegisterFresh] as soon as it has an id — before
// optionally calling Send, and well before the TUI's own follow-up Subscribe
// can possibly reach this Supervisor (see app.go's createdMsg handling: it
// switchSession/Subscribes only after Create's tea.Cmd returns) — so neither
// ever triggers a needless session/load for a session that, by construction,
// has no history yet.
func (s *Supervisor) Create(ctx context.Context, prompt string, opts tui.CreateOptions) (tui.SessionInfo, error) {
	raw, err := s.client.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: opts.Cwd, Model: opts.Model})
	if err != nil {
		return tui.SessionInfo{}, fmt.Errorf("daemonbridge: create: %w", err)
	}
	var resp newSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return tui.SessionInfo{}, fmt.Errorf("daemonbridge: decode %s response: %w", acp.MethodSessionNew, err)
	}
	s.core.RegisterFresh(resp.SessionID)

	now := time.Now()
	info := tui.SessionInfo{
		ID:      resp.SessionID,
		Model:   resp.assignedModel(opts.Model),
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

// newSessionResponse is the session/new response as this bridge reads it:
// ACP's [acp.NewSessionResponse] plus the daemon's gofer-namespaced `_meta`
// extension carrying the model it ASSIGNED (see internal/daemon's
// newSessionResult). Decoding it is what lets [Supervisor.Create] report what
// the session actually runs instead of echoing what it asked for.
type newSessionResponse struct {
	acp.NewSessionResponse
	Meta struct {
		Model string `json:"gofer/model"`
	} `json:"_meta"`
}

// assignedModel is the model the new session actually runs: the daemon's own
// answer, falling back to requested — the model this client asked for — when
// the daemon sent no `_meta` at all.
//
// The fallback is ONLY for a daemon predating the field. It must never be read
// as "the request is as good as the response": on the normal path requested is
// "" (the TUI sends no model and lets the daemon decide), which is exactly why
// echoing it left the roster row modelless (issue #162, defect 2).
func (r newSessionResponse) assignedModel(requested string) string {
	if r.Meta.Model != "" {
		return r.Meta.Model
	}
	return requested
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

// SetModel calls gofer/set_model. A cross-provider rejection (the
// supervisor's own [supervisor.ErrCrossProvider] sentinel, in-process)
// arrives here as a plain, messaged error like any other JSON-RPC
// application error — the concrete sentinel type does not survive the wire
// (see [daemon.Client.Call]'s error wrapping). A daemon-backed caller that
// needs to branch on the cross-provider case specifically (rather than just
// surface the message) should pre-check provider families itself — e.g.
// against the same model registry the SDK's provider package exposes —
// before calling, instead of trying to errors.Is against this return value.
func (s *Supervisor) SetModel(ctx context.Context, sessionID, model string) error {
	if _, err := s.client.Call(ctx, methodGoferSetModel, map[string]string{"sessionId": sessionID, "model": model}); err != nil {
		return fmt.Errorf("daemonbridge: set model %s: %w", sessionID, err)
	}
	return nil
}

// SetEffort calls gofer/set_effort. Like [Supervisor.SetModel], every
// supervisor-side sentinel ([supervisor.ErrInvalidEffort], and the SDK runner's
// own non-reasoning-model rejection) arrives here as a plain, messaged JSON-RPC
// application error — the concrete types do not survive the wire. A caller that
// needs to branch on "this model cannot reason" (the TUI's picker does, so it
// never offers levels the runner will reject) reads the same capability bit off
// [provider.Lookup] itself before calling.
//
// An empty effort is a legitimate request — the SDK's "clear back to the
// provider's default" — and is sent as such, not treated as a missing param.
func (s *Supervisor) SetEffort(ctx context.Context, sessionID, effort string) error {
	if _, err := s.client.Call(ctx, methodGoferSetEffort, map[string]string{"sessionId": sessionID, "effort": effort}); err != nil {
		return fmt.Errorf("daemonbridge: set effort %s: %w", sessionID, err)
	}
	return nil
}

// Interrupt sends session/cancel, per ACP a notification with no response —
// the in-flight session/prompt Call (see [Supervisor.Send]) resolves on its
// own once the daemon observes the cancellation, publishing the resulting
// TurnFinished(stop=cancelled) through the normal reconstruction path.
//
// ctx is honored ONLY as an admission check on the LOGICAL operation (a caller
// that has already given up need not send at all); the socket write's lifetime
// belongs to the write path, which owns its own bound (see [daemon.Client.Notify]
// — it takes no context by construction). Interrupt is the likeliest trigger for
// the borrowed-context hazard in practice — Ctrl-C then quit cancels the peer
// request that carried the session/cancel — so handing ctx to the write would
// have let the quit tear down the shared daemon link mid-write.
func (s *Supervisor) Interrupt(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.client.Notify(acp.MethodSessionCancel, acp.CancelNotification{SessionID: sessionID}); err != nil {
		return fmt.Errorf("daemonbridge: interrupt %s: %w", sessionID, err)
	}
	return nil
}

// Reply answers a pending permission request by sending [methodPermissionReply]
// — a bare notification, matching the "permission.reply" op's own
// fire-and-forget contract (see event.PermissionReply's doc: it carries no
// response). sessionID is not part of the wire payload: the daemon resolves
// a request by id alone (see the reconstruction core's reconstruction — the
// same id [event.PermissionRequested]/[event.PermissionResolved] already
// carry), matching [tui.Supervisor.Reply]'s doc.
//
// As with [Supervisor.Interrupt], ctx is honored only as an admission check on
// the logical operation; the write's lifetime is owned by [daemon.Client.Notify]
// (which takes no context), so a caller cancellation cannot close the shared
// daemon link mid-write.
func (s *Supervisor) Reply(ctx context.Context, sessionID, id string, allow, remember bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	verdict := event.VerdictDeny
	if allow {
		verdict = event.VerdictAllow
	}
	params := struct {
		ID       string        `json:"id"`
		Verdict  event.Verdict `json:"verdict"`
		Remember bool          `json:"remember,omitempty"`
	}{ID: id, Verdict: verdict, Remember: remember}
	if err := s.client.Notify(methodPermissionReply, params); err != nil {
		return fmt.Errorf("daemonbridge: reply %s (session %s): %w", id, sessionID, err)
	}
	return nil
}

// ErrDecisionsUnsupported reports that the daemon-backed path cannot carry a
// structured decision yet. The relay — a gofer/decision_requested notification
// out of the daemon and a decision.answer op back in, mirroring the permission
// pair above — is the follow-up PR for #173; this build ships the in-process
// (internal/tuibridge) path only.
var ErrDecisionsUnsupported = errors.New("daemonbridge: structured decisions require the daemon relay (follow-up PR for #173)")

// Decisions returns an already-closed subscription: a daemon-backed session's
// decision requests do not reach a client until the relay lands (see
// [ErrDecisionsUnsupported]). Closed rather than merely idle, and nil rather
// than an error, on purpose — the TUI's pump treats a closed channel as "this
// stream is over" and stops re-arming, so a daemon-backed attach costs one
// subscribe and nothing else, while returning an error here would surface a
// failure banner for a feature the user never invoked.
//
// It is a real [decision.Subscription] over a throwaway gate rather than a
// hand-rolled zero value: Close is what closes C, and going through the gate
// keeps this stub honest against the package's actual lifecycle.
func (s *Supervisor) Decisions(ctx context.Context, _ string) (*decision.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sub := decision.NewGate("").Subscribe(0)
	sub.Close()
	return sub, nil
}

// AnswerDecision always fails with [ErrDecisionsUnsupported]: with no relay
// there is no gate on the far side to route an answer to. It reports that
// plainly rather than returning nil — a silent success would let a client
// believe an agent turn had been unblocked when it is still waiting.
func (s *Supervisor) AnswerDecision(context.Context, string, string, []acp.DecisionAnswer) error {
	return ErrDecisionsUnsupported
}
