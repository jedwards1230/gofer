// Package tuibridge adapts the daemon's [*supervisor.Supervisor] to the TUI's
// narrow [tui.Supervisor] consumer interface. It is the single seam importing
// both packages, so internal/tui stays free of the supervisor→runner→session
// dependency chain — the TUI depends only on the interface it declares, and
// this bridge maps the concrete daemon type onto it.
//
// The two packages' SessionInfo/SessionStatus are structurally identical for
// the fields the TUI reads; the adapter copies them across, dropping the
// supervisor's operational extras (Project/JournalPath/Queued/Live) the TUI has
// no use for. When the planned reconciliation lands — the TUI importing the
// supervisor's value types directly — this mapping collapses and the adapter
// becomes a plain pass-through, or disappears entirely.
package tuibridge

import (
	"context"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

// Adapter presents a [*supervisor.Supervisor] as a [tui.Supervisor].
type Adapter struct {
	sup *supervisor.Supervisor
	// defaultModel RESOLVES the model a Create with no explicit model falls
	// back to — this adapter's equivalent of [daemon.Config.DefaultModel] on
	// the daemon path. Without it the TUI's own resolved model never reached
	// Create and every session started from the roster died on an empty model
	// id (issue #147).
	//
	// It is a function, not a captured string, and that is the whole point
	// (issue #156). A string is a COPY of the default taken at construction:
	// once `/model` writes a new session.model into config.json, the copy is
	// stale for the rest of the process's life, so every session the TUI
	// created after a `/model` still ran the model the process started with
	// and no amount of reselecting could change it. Resolving on each Create
	// keeps exactly one source of truth — whatever the resolver reads — so a
	// change made anywhere (this TUI's `/model`, an edit to config.json, a
	// `gofer login` in another terminal) is picked up with no restart.
	//
	// nil, or a resolver returning "", is valid: Create then fails with
	// supervisor.ErrNoModel, whose message names the remedy.
	defaultModel func(context.Context) string
}

// New returns an Adapter wrapping sup, resolving the create-time fallback
// model through defaultModel whenever the caller supplies no model of its own.
// defaultModel is called on each such Create — see [Adapter.defaultModel] for
// why it must not be a value captured once.
func New(sup *supervisor.Supervisor, defaultModel func(context.Context) string) Adapter {
	return Adapter{sup: sup, defaultModel: defaultModel}
}

// Adapter satisfies the TUI's consumer interface. Failing this assertion means
// the supervisor's method set drifted from what the TUI drives.
var _ tui.Supervisor = Adapter{}

// Roster maps the supervisor's live roster to the TUI's row type.
func (a Adapter) Roster(ctx context.Context) ([]tui.SessionInfo, error) {
	infos, err := a.sup.Roster(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tui.SessionInfo, len(infos))
	for i := range infos {
		out[i] = toTUI(infos[i])
	}
	return out, nil
}

// Subscribe passes through — the *event.Subscription is the same SDK type on
// both sides.
func (a Adapter) Subscribe(ctx context.Context, sessionID string) (*event.Subscription, error) {
	return a.sup.Subscribe(ctx, sessionID)
}

// Create maps the TUI's create options onto the supervisor's and returns the
// new session's TUI row. An unset opts.Model falls back to the adapter's
// defaultModel resolver, mirroring how the daemon resolves session/new against
// [daemon.Config.DefaultModel] — resolved HERE, per create, so a default
// changed since this adapter was built (via `/model`, say) applies to this
// session rather than to the next process.
func (a Adapter) Create(ctx context.Context, prompt string, opts tui.CreateOptions) (tui.SessionInfo, error) {
	model := opts.Model
	if model == "" && a.defaultModel != nil {
		model = a.defaultModel(ctx)
	}
	info, err := a.sup.Create(ctx, prompt, supervisor.CreateOptions{
		Model:    model,
		Cwd:      opts.Cwd,
		ParentID: opts.ParentID,
		Agent:    opts.Agent,
	})
	if err != nil {
		return tui.SessionInfo{}, err
	}
	return toTUI(info), nil
}

// ListSessions maps the supervisor's store-wide enumeration — every session on
// disk, live and offline alike ([supervisor.Supervisor.List]) — to the TUI's
// resume-picker row. Only the four fields a disk-only session can honestly
// answer cross over; the operational extras a live row also carries (Status,
// Cost, Usage) are deliberately dropped rather than shown as zeroes for the
// offline majority (see [tui.SessionRef]).
func (a Adapter) ListSessions(ctx context.Context) ([]tui.SessionRef, error) {
	infos, err := a.sup.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tui.SessionRef, len(infos))
	for i, info := range infos {
		out[i] = tui.SessionRef{ID: info.ID, Title: info.Title, Cwd: info.Cwd, Updated: info.Updated}
	}
	return out, nil
}

// Resume reopens sessionID as a live session in cwd. The model is resolved the
// same per-call way [Adapter.Create] resolves it — the supervisor requires one
// on every Resume (the journal does not persist it, see
// [supervisor.ResumeOptions]) and reading it now rather than at construction
// keeps a `/model` write made since made effective here too.
func (a Adapter) Resume(ctx context.Context, sessionID, cwd string) error {
	var model string
	if a.defaultModel != nil {
		model = a.defaultModel(ctx)
	}
	_, err := a.sup.Resume(ctx, sessionID, supervisor.ResumeOptions{Cwd: cwd, Model: model})
	return err
}

// Send submits prompt as sessionID's next turn.
func (a Adapter) Send(ctx context.Context, sessionID, prompt string) error {
	return a.sup.Send(ctx, sessionID, prompt)
}

// Interrupt stops sessionID's in-flight turn.
func (a Adapter) Interrupt(ctx context.Context, sessionID string) error {
	return a.sup.Interrupt(ctx, sessionID)
}

// Kill interrupts and terminates sessionID.
func (a Adapter) Kill(ctx context.Context, sessionID string) error {
	return a.sup.Kill(ctx, sessionID)
}

// Archive drops sessionID from the roster.
func (a Adapter) Archive(ctx context.Context, sessionID string) error {
	return a.sup.Archive(ctx, sessionID)
}

// SetModel passes through to the supervisor's own SetModel. An in-process
// caller gets back the real [supervisor.ErrCrossProvider] sentinel
// unwrapped (errors.Is works directly), unlike a daemon-backed
// [daemonbridge.Supervisor], which only ever sees a plain messaged error
// (see that package's SetModel doc).
func (a Adapter) SetModel(ctx context.Context, sessionID, model string) error {
	return a.sup.SetModel(ctx, sessionID, model)
}

// SetEffort passes through to the supervisor's own SetEffort. An in-process
// caller gets back the real [supervisor.ErrInvalidEffort] sentinel unwrapped
// (errors.Is works directly), unlike a daemon-backed
// [daemonbridge.Supervisor], which only ever sees a plain messaged error.
func (a Adapter) SetEffort(ctx context.Context, sessionID, effort string) error {
	return a.sup.SetEffort(ctx, sessionID, effort)
}

// Reply answers a pending permission request by routing straight to the
// supervisor's own Reply, which resolves the session's loop.Gate — see
// internal/supervisor's Reply doc. ctx is accepted to satisfy
// [tui.Supervisor] (every other method here takes one), though the
// supervisor's own Reply is synchronous and never blocks on I/O (routing to
// an in-memory Gate), so there is nothing for it to cancel.
//
// d.Input rides along verbatim as [event.PermissionReply.Input] — the
// amended tool input an in-process session runs with instead of the model's
// original arguments. It is nil for a plain allow/deny, leaving that path
// byte-identical to before amend existed.
func (a Adapter) Reply(_ context.Context, sessionID, id string, d tui.PermissionDecision) error {
	verdict := event.VerdictDeny
	if d.Allow {
		verdict = event.VerdictAllow
	}
	return a.sup.Reply(sessionID, event.PermissionReply{ID: id, Verdict: verdict, Remember: d.Remember, Input: d.Input})
}

// ExplainPermission passes through to the supervisor's own read-only
// rationale lookup — the in-process counterpart of the ACP
// session/explain_permission round trip [daemonbridge.Supervisor] makes, and
// the same [permrationale] grammar behind both, so a daemonless TUI explains a
// gated call exactly as an attached one does.
//
// It is called DIRECTLY on the concrete supervisor rather than through any
// capability interface: the daemon answers explains from its own retained
// requests (see internal/daemon's handleExplainPermission), so nothing about
// this method belongs on [daemon.Supervisor]. ctx is accepted to satisfy
// [tui.Supervisor] and unused for the same reason [Adapter.Reply]'s is — the
// lookup is an in-memory map read with nothing to cancel.
func (a Adapter) ExplainPermission(_ context.Context, sessionID, callID string) (acp.PermissionRationale, error) {
	return a.sup.ExplainPermission(sessionID, callID)
}

// decisionBuffer sizes the decision subscription this adapter hands the TUI.
// A session has one outstanding decision at a time in practice, so the buffer
// exists only to absorb a burst while the TUI is mid-frame; the gate drops
// (and counts) rather than blocking when it fills, so an over-small buffer
// would cost a missed prompt, not a wedged turn.
const decisionBuffer = 8

// Decisions subscribes to sessionID's open structured-decision requests
// straight through the supervisor's own gate — the in-process path shares
// memory with it, so no reconstruction is needed (contrast
// internal/daemonbridge). ctx is accepted to satisfy [tui.Supervisor]; the
// subscribe itself is an in-memory registration with nothing to cancel, and
// the returned subscription's lifetime is the caller's (Close it).
func (a Adapter) Decisions(_ context.Context, sessionID string) (*decision.Subscription, error) {
	return a.sup.SubscribeDecisions(sessionID, decisionBuffer)
}

// AnswerDecision resolves an outstanding decision request by routing to the
// supervisor's own AnswerDecision, which validates the answers against the
// request and unblocks the ask_user tool call waiting on it. As with [Reply],
// ctx is accepted for interface conformance only — the call is synchronous
// in-memory routing.
func (a Adapter) AnswerDecision(_ context.Context, sessionID, requestID string, answers []acp.DecisionAnswer) error {
	return a.sup.AnswerDecision(sessionID, requestID, answers)
}

// toTUI copies the fields the TUI renders from a supervisor snapshot. The
// status cast relies on the two SessionStatus enums sharing ordinals — a
// property the mapping test pins so a future drift fails loudly.
func toTUI(s supervisor.SessionInfo) tui.SessionInfo {
	return tui.SessionInfo{
		ID:        s.ID,
		Title:     s.Title,
		Summary:   s.Summary,
		Status:    tui.SessionStatus(s.Status),
		Model:     s.Model,
		Effort:    s.Effort,
		Cwd:       s.Cwd,
		Cost:      s.Cost,
		Usage:     s.Usage,
		Pending:   s.Pending,
		Artifacts: s.Artifacts,
		Created:   s.Created,
		Updated:   s.Updated,
		// Live-only under M6: an in-process supervisor leaves it empty (there is
		// no separate worker process to have its own build), a router stamps it
		// from the owning worker's gofer/hello.
		BinaryVersion: s.BinaryVersion,
		// The subagent link. Zero for a root session — which is every session
		// created without an explicit parent, so this mapping is inert for the
		// existing roster.
		ParentID: s.ParentID,
		Agent:    s.Agent,
		Depth:    s.Depth,
	}
}
