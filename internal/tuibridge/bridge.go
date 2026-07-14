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

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

// Adapter presents a [*supervisor.Supervisor] as a [tui.Supervisor].
type Adapter struct{ sup *supervisor.Supervisor }

// New returns an Adapter wrapping sup.
func New(sup *supervisor.Supervisor) Adapter { return Adapter{sup: sup} }

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
// new session's TUI row.
func (a Adapter) Create(ctx context.Context, prompt string, opts tui.CreateOptions) (tui.SessionInfo, error) {
	info, err := a.sup.Create(ctx, prompt, supervisor.CreateOptions{Model: opts.Model, Cwd: opts.Cwd})
	if err != nil {
		return tui.SessionInfo{}, err
	}
	return toTUI(info), nil
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

// Reply answers a pending permission request by routing straight to the
// supervisor's own Reply, which resolves the session's loop.Gate — see
// internal/supervisor's Reply doc. ctx is accepted to satisfy
// [tui.Supervisor] (every other method here takes one), though the
// supervisor's own Reply is synchronous and never blocks on I/O (routing to
// an in-memory Gate), so there is nothing for it to cancel.
func (a Adapter) Reply(_ context.Context, sessionID, id string, allow, remember bool) error {
	verdict := event.VerdictDeny
	if allow {
		verdict = event.VerdictAllow
	}
	return a.sup.Reply(sessionID, event.PermissionReply{ID: id, Verdict: verdict, Remember: remember})
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
		Cwd:       s.Cwd,
		Cost:      s.Cost,
		Usage:     s.Usage,
		Pending:   s.Pending,
		Artifacts: s.Artifacts,
		Created:   s.Created,
		Updated:   s.Updated,
	}
}
