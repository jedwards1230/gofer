package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// routerCallTimeout bounds a worker→router control call (session/new). It
// matches internal/router's own wireCallTimeout: the same hop, in the other
// direction, and a wedged peer must not hold a turn's tool call open.
const routerCallTimeout = 15 * time.Second

// routerDialTimeout bounds the dial-back to the router.
const routerDialTimeout = 10 * time.Second

// RouterSubagents is the `--workers` implementation of
// [supervisor.Subagents]: a worker dials the ROUTER to create child sessions
// and to deliver a finished child's report.
//
// # Why the router creates every child, and a worker never does
//
// A worker's embedded daemon is built with MaxSessions: 1 — a worker IS a
// single-session daemon (see this package's doc). It structurally cannot host a
// sibling session, so "spawn locally" is not an option that exists. The router
// already owns one-worker-per-session bring-up (spawnWorker/buildWorkerCmd), so
// routing every child through it keeps exactly ONE place in gofer that mints
// sessions and forks processes — the same place that enforces MaxWorkers, and
// the same place that would have to be taught about a second one otherwise.
//
// # Why the dial-back cannot deadlock
//
// A worker only exists because the router's daemon is already SERVING the
// session/new RPC that spawned it, so the listener is up by construction. This
// dial is an independent client connection, not a nested call inside the
// router→worker connection, and neither router.Supervisor.Create nor spawnWorker
// holds a lock across a wire round trip.
//
// # Prompts are fired, not awaited
//
// session/prompt blocks server-side for the WHOLE turn. Both the child's first
// prompt and a report to a parent therefore run on their own goroutine — the
// same fire-and-forget shape [wirestream.Reconstructor.Send] uses — so a spawn
// returns as soon as the child exists and a report returns as soon as it is on
// the wire. Blocking either on the far session's turn would pin this worker's
// pump for as long as another session takes to think.
type RouterSubagents struct {
	addr  string
	token string
	// sup resolves this worker's own supervisor, consulted ONLY to read the
	// parent session's model and cwd so the child inherits them (the parent is
	// this worker's single session). It is never used to create anything.
	//
	// A getter rather than the value because of a construction order the seam
	// cannot avoid: supervisor.Config takes the seam BY VALUE, so the seam must
	// exist before the supervisor it reads from does. The same shape cmd/gofer's
	// runDaemon uses for its out-of-turn event relay's `var d *daemon.Daemon`.
	// It may return nil (or be nil itself) — see inherit's fallback.
	sup func() *supervisor.Supervisor
	log *slog.Logger

	mu     sync.Mutex
	client *daemon.Client
	closed bool
}

// RouterSubagents satisfies the seam — a signature drift fails the build here
// rather than at supervisor.New.
var _ supervisor.Subagents = (*RouterSubagents)(nil)

// NewRouterSubagents builds the seam for a worker whose router listens at addr
// with the given bearer token (empty for the loopback default). sup resolves the
// worker's own supervisor when one exists — it is read at CALL time, so a caller
// may hand over a closure that is filled in after this returns (see the field's
// doc); nil is legal and costs only the model/cwd inheritance. logger may be nil.
//
// It does NOT dial: the connection is opened lazily on first use and re-opened
// if it drops, so a worker whose session never spawns anything pays nothing, and
// a router restart between spawns is invisible.
func NewRouterSubagents(addr, token string, sup func() *supervisor.Supervisor, logger *slog.Logger) *RouterSubagents {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &RouterSubagents{addr: addr, token: token, sup: sup, log: logger}
}

// Spawn creates the child on the ROUTER (session/new carrying the gofer/parent
// and gofer/agent `_meta` the wire already defines — no new protocol), then
// fires its first prompt. The router forwards the link to the child's own
// worker, whose supervisor resolves the parent against the SHARED store root and
// derives the depth, so the cap is enforced exactly where it is on the
// in-process path.
func (r *RouterSubagents) Spawn(ctx context.Context, parentID, agent, prompt string) (string, error) {
	client, err := r.dial(ctx)
	if err != nil {
		return "", fmt.Errorf("worker: spawn subagent of %s: %w", parentID, err)
	}
	model, cwd := r.inherit(ctx, parentID)

	callCtx, cancel := context.WithTimeout(ctx, routerCallTimeout)
	raw, err := client.Call(callCtx, acp.MethodSessionNew,
		daemon.NewSessionRequestFor(cwd, model, parentID, agent))
	cancel()
	if err != nil {
		return "", fmt.Errorf("worker: spawn subagent of %s: session/new on router: %w", parentID, err)
	}
	var resp daemon.NewSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("worker: spawn subagent of %s: decode %s response: %w", parentID, acp.MethodSessionNew, err)
	}
	if resp.SessionID == "" {
		return "", fmt.Errorf("worker: spawn subagent of %s: router returned an empty session id", parentID)
	}
	r.firePrompt(client, resp.SessionID, prompt)
	return resp.SessionID, nil
}

// Report delivers a finished child's result to its parent as that parent's next
// prompt, routed by the router to whichever worker hosts it. Queued, never
// interrupting — the parent's own supervisor decides when to run it.
func (r *RouterSubagents) Report(ctx context.Context, parentID, text string) error {
	client, err := r.dial(ctx)
	if err != nil {
		return fmt.Errorf("worker: report to parent %s: %w", parentID, err)
	}
	r.firePrompt(client, parentID, text)
	return nil
}

// Close shuts the dial-back connection down. Safe to call more than once, and
// safe to call with no connection ever opened.
func (r *RouterSubagents) Close() error {
	r.mu.Lock()
	client := r.client
	r.client = nil
	r.closed = true
	r.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.Close()
}

// firePrompt sends prompt to sessionID on its own goroutine — see the type doc
// for why it is never awaited. An empty prompt is a no-op (a spawn with no brief
// creates an idle child, matching Supervisor.Create's own empty-prompt path).
//
// context.Background, not a caller ctx: the turn this starts outlives the tool
// call that started it, exactly as [wirestream.Reconstructor.Send]'s does. A
// failure is logged rather than returned — there is nobody left to return it to.
func (r *RouterSubagents) firePrompt(client *daemon.Client, sessionID, prompt string) {
	if prompt == "" {
		return
	}
	go func() {
		_, err := client.Call(context.Background(), acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sessionID,
			Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
		})
		if err != nil {
			r.log.Warn("subagent prompt through router failed", "session", sessionID, "err", err)
		}
	}()
}

// inherit reads the parent session's model and cwd off this worker's own roster
// so the child runs the same model in the same directory (see
// [supervisor.localSubagents.Spawn] for why inheritance rather than
// configuration). Both fall back to empty, which the router resolves to its own
// default model and working directory — a worse answer than inheritance, but a
// working one, and never a reason to refuse a spawn.
func (r *RouterSubagents) inherit(ctx context.Context, parentID string) (model, cwd string) {
	if r.sup == nil {
		return "", ""
	}
	sup := r.sup()
	if sup == nil {
		return "", ""
	}
	rows, err := sup.Roster(ctx)
	if err != nil {
		return "", ""
	}
	for _, row := range rows {
		if row.ID == parentID {
			return row.Model, row.Cwd
		}
	}
	return "", ""
}

// dial returns a live client to the router, opening one on first use and
// re-opening it if the previous connection has closed (a router restart between
// spawns). The inbound notification channel is drained for the connection's
// whole life: [daemon.Client]'s read loop BLOCKS on delivering a notification,
// so a client nobody drains wedges after the first session/update push — and
// this connection receives them, since session/new subscribes the caller.
func (r *RouterSubagents) dial(ctx context.Context) (*daemon.Client, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("subagent router link is closed")
	}
	if c := r.client; c != nil {
		select {
		case <-c.Done():
			// The previous connection died; drop it and dial a fresh one below.
			r.client = nil
		default:
			r.mu.Unlock()
			return c, nil
		}
	}
	r.mu.Unlock()

	dialCtx, cancel := context.WithTimeout(ctx, routerDialTimeout)
	client, err := daemon.Dial(dialCtx, r.addr, r.token)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("dial router %s: %w", r.addr, err)
	}
	go func() {
		for range client.Notifications() {
		}
	}()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		_ = client.Close()
		return nil, errors.New("subagent router link is closed")
	}
	if r.client != nil {
		// Lost a race with a concurrent dial: keep the winner, close ours.
		_ = client.Close()
		return r.client, nil
	}
	r.client = client
	return client, nil
}
