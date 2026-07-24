package decision

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// Gate is one session's registry of outstanding structured-decision requests —
// the decision-side analogue of the SDK's loop.Gate for permissions. The
// `ask_user` tool opens a request with [Gate.Request] (blocking the agent's
// turn on the calling goroutine); clients watch the open set with
// [Gate.Subscribe] and resolve a request with [Gate.Answer].
//
// Like loop.Gate, Request spawns no goroutine: it blocks the caller on a select
// between a buffered answer channel, ctx.Done, and the gate's own shutdown, so
// an interrupted or torn-down turn releases the waiter and leaves nothing
// running. Unlike loop.Gate there is no "answer arrived before the waiter
// registered" window to stash: the request id is minted inside Request, under
// the same lock that publishes it, so no client can name a request before its
// waiter exists.
//
// A Gate lives exactly as long as its session: [Gate.Close] ends it, dropping
// the open requests and closing every subscription.
//
// The zero value is not usable; build one with [NewGate]. Every method is safe
// for concurrent use.
type Gate struct {
	mu sync.Mutex
	// sessionID is stamped onto every Request so a client that multiplexes
	// several sessions knows which one is blocked. It is late-bound on the
	// create path — see [Gate.Bind].
	sessionID string
	// seq mints request ids. Monotonic per gate, never reused, so "dec-3"
	// means the same request for the life of the session.
	seq int
	// open holds every request currently awaiting an answer, keyed by id;
	// order keeps their open order for a deterministic replay and snapshot.
	// Both are maintained together — a request leaves the two atomically,
	// under mu, exactly once (whichever of Answer / a giving-up waiter (see
	// settle) / Close gets there first).
	open  map[string]*pending
	order []string
	// fan is this gate's subscriber registry, shared in TYPE (not in state) with
	// the client-side [Stream] so both hand out the same [Subscription] — see
	// [fanout]. Every publish into it happens under mu, which is what keeps an
	// update's ordering identical to the state change that produced it.
	fan fanout
	// closed reports that [Gate.Close] has run: the session is gone, so no new
	// request may open and no new subscription may register. Guarded by mu;
	// done is its lock-free twin for the blocked waiters in Request, which
	// cannot re-take mu while parked.
	closed bool
	// done is closed exactly once, by Close, to release every blocked
	// [Gate.Request] with [ErrClosed].
	done chan struct{}
}

// pending is one open request plus the channel its blocked [Gate.Request] is
// selecting on. answers is buffered (size 1) so [Gate.Answer] never blocks —
// including when the waiter has already given up on ctx and nobody will ever
// receive.
type pending struct {
	req     Request
	answers chan []acp.DecisionAnswer
}

// NewGate returns a Gate for sessionID. Pass "" when the session's id is not
// minted yet and call [Gate.Bind] once it is.
func NewGate(sessionID string) *Gate {
	return &Gate{
		sessionID: sessionID,
		open:      make(map[string]*pending),
		done:      make(chan struct{}),
	}
}

// Bind stamps the session id onto a gate built before its session existed.
// The supervisor's create path needs it: the tool registry (which closes over
// this gate) must be handed to runner.New, and only that call mints the
// session id — so the gate is necessarily constructed a moment early. It is
// called once, at registration, before any turn can run; a later call
// overwrites the id but does NOT rewrite requests already open, which is why
// it must not be used as a general setter.
func (g *Gate) Bind(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessionID = sessionID
}

// Request opens a decision, notifies every subscriber, and blocks until a
// client answers it or ctx is done.
//
// It returns [ErrNoClient] immediately — without opening anything — when no
// subscriber is attached, because a question nobody can see would otherwise
// hang the turn for as long as the agent's context lives. The check is
// inherently racy at its edges (a client may detach a microsecond later); that
// residual case is what ctx cancellation is for.
//
// # ErrNoClient means "no SUBSCRIBER", which under a daemon is always false
//
// The check counts subscribers, not humans. On the daemonless path that
// distinction does not exist — the only subscriber is the attached TUI — so a
// session nobody is watching gets ErrNoClient and the tool tells the model to
// continue in prose. Under a daemon it does: the supervisor installs its OWN
// standing watcher per session (see internal/supervisor's SetDecisionRelay),
// because the daemon has to observe a request to put it on the wire at all.
// That watcher is a subscriber, so ErrNoClient never fires in daemon mode and a
// decision asked with ZERO peers attached simply stays open until a peer
// attaches (session/load replays it), the turn is interrupted, or the session
// ends.
//
// That is deliberate, and it is exactly what a PERMISSION asked with zero peers
// attached already does today — it blocks on its gate until someone answers or
// the turn is cancelled. Both are "the agent needs a human; wait for one",
// which is the consistent behavior for a supervisor whose whole purpose is that
// clients come and go. There is no decision-side timeout, and inventing one
// would silently answer a question the user never saw.
//
// The returned answers are normalized by [Gate.Answer]: exactly one per
// question, in question order, with anything the client left out filled in as
// [acp.DecisionOutcomeCancelled]. On ctx cancellation the request is dropped
// from the open set and an [UpdateResolved] is published, so a client
// rendering the prompt clears it rather than answering a request that no
// longer has a waiter. On [Gate.Close] — the session itself going away — a
// blocked Request returns [ErrClosed].
func (g *Gate) Request(ctx context.Context, questions []acp.DecisionQuestion) ([]acp.DecisionAnswer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("decision: request: no questions")
	}

	// Re-stamp unconditionally: ids are gofer's to mint (see AssignIDs), and
	// re-stamping an already-stamped batch is a no-op.
	stamped := AssignIDs(questions)
	p := &pending{answers: make(chan []acp.DecisionAnswer, 1)}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, ErrClosed
	}
	if g.fan.count() == 0 {
		g.mu.Unlock()
		return nil, ErrNoClient
	}
	g.seq++
	p.req = Request{ID: fmt.Sprintf("dec-%d", g.seq), SessionID: g.sessionID, Questions: stamped}
	g.open[p.req.ID] = p
	g.order = append(g.order, p.req.ID)
	g.publishLocked(Update{Kind: UpdateRequested, Request: p.req})
	g.mu.Unlock()

	select {
	case answers := <-p.answers:
		return answers, nil
	case <-g.done:
		if answers, ok := g.settle(p); ok {
			return answers, nil
		}
		return nil, ErrClosed
	case <-ctx.Done():
		if answers, ok := g.settle(p); ok {
			return answers, nil
		}
		return nil, ctx.Err()
	}
}

// settle finishes a request whose waiter is giving up — ctx cancelled, or the
// gate closed. It returns the answers when one arrived anyway, in which case the
// caller must return them rather than its own error.
//
// Deciding this under mu is what makes the hand-off airtight. [Gate.Answer]
// removes the request and delivers its answers in one critical section, so the
// two possibilities here are totally ordered: either the request is still open,
// in which case no Answer can ever resolve it (this call removes it first), or
// it is not, in which case an Answer already reported success to its client and
// its answers are sitting in the buffer waiting to be honored. Draining outside
// the lock instead — or letting the select pick between two ready cases, which
// it does uniformly at random — would discard that answer on a coin flip while
// the client was told it succeeded.
func (g *Gate) settle(p *pending) ([]acp.DecisionAnswer, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, open := g.open[p.req.ID]; open {
		g.removeLocked(p.req.ID)
		// Tell every client the prompt is stale: nothing is waiting on it now.
		g.publishLocked(Update{Kind: UpdateResolved, Request: Request{ID: p.req.ID, SessionID: g.sessionID}})
		return nil, false
	}
	// Already out of the open set: an Answer beat us to it (its send is in the
	// buffer) or Close dropped it (nothing to take).
	select {
	case answers := <-p.answers:
		return answers, true
	default:
		return nil, false
	}
}

// Answer resolves the outstanding request requestID with the client's answers.
// It returns [ErrUnknownRequest] for an id that is not open, and a descriptive
// error for an answer that does not fit the request: an unknown question id, a
// second answer for a question already answered, an answer with no outcome at
// all, or an outcome the question does not accept (see [validateOutcome] — a
// missing option, an affordance the model opted out of, or a variant outside
// acp's four). A rejected call leaves the request open, so the client can
// correct and retry.
//
// Validating here rather than in [Gate.Request]'s caller is deliberate: this is
// the seam untrusted-ish input (a peer's answer over the daemon wire — see
// internal/daemon's decision.answer op) enters through, and the tool downstream
// must be able to trust
// that every answer references a real question and a real option.
func (g *Gate) Answer(requestID string, answers []acp.DecisionAnswer) error {
	g.mu.Lock()
	p, ok := g.open[requestID]
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("decision: answer %s: %w", requestID, ErrUnknownRequest)
	}
	normalized, err := normalize(p.req.Questions, answers)
	if err != nil {
		g.mu.Unlock()
		return fmt.Errorf("decision: answer %s: %w", requestID, err)
	}
	g.removeLocked(requestID)
	g.publishLocked(Update{Kind: UpdateResolved, Request: Request{ID: requestID, SessionID: g.sessionID}})
	// Delivered under mu, deliberately: the removal above means exactly one
	// Answer can ever reach this send for a given id, and the channel is
	// buffered, so it cannot block — and doing it inside the same critical
	// section is what lets a waiter giving up on ctx decide, under that same
	// lock, whether an answer is still coming (see settle).
	p.answers <- normalized
	g.mu.Unlock()
	return nil
}

// Open returns a snapshot of the currently-open requests, in the order they
// were opened. Each Request shares its Questions slice with the live request;
// treat it as read-only.
//
// It is the "what is this session blocked on right now?" query — used by tests
// and by the daemon relay's replay-on-attach (internal/daemon's
// pendingDecisionsForSession). Note that no
// client reconciles against it today: [Gate.Subscribe] already replays the open
// set on attach, which covers every case except a subscriber that let its own
// buffer overflow (see [Subscription.Dropped]).
func (g *Gate) Open() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.openLocked()
}

// Subscribe returns a stream of [Update]s, replaying an [UpdateRequested] for
// every currently-open request before any live update — so a client attaching
// mid-flight still sees the prompt the agent is blocked on. buffer sizes the
// channel; a negative value is clamped to 0, and the replay is always
// guaranteed room (matching event.Broker.Subscribe).
//
// Delivery is drop-on-full with a counter ([Subscription.Dropped]), NOT
// blocking: a wedged client must never be able to hang an agent turn inside
// [Gate.Request]. The cost is that a client which lets its buffer fill can miss
// a prompt. Nothing reconciles that today — buffer being large enough is the
// whole mitigation, and a session has one outstanding decision at a time in
// practice. [Subscription.Dropped] and [Gate.Open] are what a reconcile would
// be built from if that ever stops holding.
// Subscribing to a closed gate returns an already-closed subscription rather
// than an error, so a client pump learns the session is gone the same way it
// learns a live subscription ended — one code path, not two.
func (g *Gate) Subscribe(buffer int) *Subscription {
	if buffer < 0 {
		buffer = 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.closed {
		ch := make(chan Update)
		sub := &Subscription{C: ch, ch: ch, src: &g.fan}
		sub.once.Do(func() { close(ch) })
		return sub
	}

	replay := g.openLocked()
	capacity := buffer
	if len(replay) > capacity {
		capacity = len(replay)
	}
	ch := make(chan Update, capacity)
	sub := &Subscription{C: ch, ch: ch, src: &g.fan}
	for _, r := range replay {
		ch <- Update{Kind: UpdateRequested, Request: r} // fits: capacity >= len(replay)
	}
	g.fan.add(sub)
	return sub
}

// Close shuts the gate down for good: every open request is dropped (each
// publishing its [UpdateResolved] first, so a client still rendering the prompt
// clears it), every blocked [Gate.Request] is released with [ErrClosed], and
// every subscription's channel is closed so its consumer's pump unwinds instead
// of parking on a channel nothing will ever publish to again.
//
// It is the decision-side analogue of closing a session's event broker, and the
// supervisor calls it from the same place (managed.stop) for the same reason:
// without it, killing an attached session leaves the client's decision reader
// blocked forever, and — because the gate's own ErrNoClient check counts
// subscribers — the dead subscription would keep claiming a client is watching.
//
// The ORDER of those first two — a resolution for every open request, and only
// THEN the closed channels — is load-bearing well beyond clearing a widget, so
// a rewrite must preserve it. internal/supervisor's watchDecisions deliberately
// does NOT exit on its session's base context (unlike its permission twin)
// precisely so it is still draining this subscription when Close publishes:
// those resolutions are what release the host's per-request bookkeeping —
// internal/daemon's decisionRoutes, pendingDecisions, and decisionReqCancels. A
// Close that dropped the open set without publishing, or closed the
// subscriptions first, would silently leak all three for every request open on
// a killed session. TestGateCloseResolvesEveryOpenRequestBeforeClosingSubscriptions
// is the direct guard.
//
// Close is idempotent and safe to call concurrently with Request/Answer/
// Subscribe: it takes mu for the state transition, and the subscription
// channels are closed only after their subscriptions have been removed from the
// set publishLocked walks, so no send can race a close.
func (g *Gate) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	for _, id := range slices.Clone(g.order) {
		g.removeLocked(id)
		g.publishLocked(Update{Kind: UpdateResolved, Request: Request{ID: id, SessionID: g.sessionID}})
	}
	subs := g.fan.drain()
	close(g.done)
	g.mu.Unlock()

	// Outside mu: a Subscription.Close racing this one also takes mu, and the
	// sync.Once is what makes exactly one of the two close the channel.
	for _, sub := range subs {
		sub.once.Do(func() { close(sub.ch) })
	}
}

// removeLocked deletes requestID from both the open map and the order slice.
// Callers hold g.mu.
func (g *Gate) removeLocked(requestID string) {
	delete(g.open, requestID)
	for i, id := range g.order {
		if id == requestID {
			g.order = append(g.order[:i], g.order[i+1:]...)
			break
		}
	}
}

// openLocked snapshots the open requests in open order. Callers hold g.mu.
func (g *Gate) openLocked() []Request {
	if len(g.order) == 0 {
		return nil
	}
	out := make([]Request, 0, len(g.order))
	for _, id := range g.order {
		if p, ok := g.open[id]; ok {
			out = append(out, p.req)
		}
	}
	return out
}

// publishLocked fans u out to every subscriber, dropping (and counting) on a
// full buffer rather than blocking — see [fanout.publish]. Callers hold g.mu,
// which is safe precisely because the send never blocks, and NECESSARY because
// publishing under the same lock that changed the open set is what keeps a
// client's view of that set ordered the same way the gate's own is.
func (g *Gate) publishLocked(u Update) {
	g.fan.publish(u)
}

// normalize validates answers against questions and returns exactly one answer
// per question, in question order. A question the client left unanswered comes
// back as [acp.DecisionOutcomeCancelled] — so the tool downstream can format
// its result by iterating questions, with no "was this one answered?" branch
// and no chance of a nil outcome reaching it.
func normalize(questions []acp.DecisionQuestion, answers []acp.DecisionAnswer) ([]acp.DecisionAnswer, error) {
	byID := make(map[string]acp.DecisionQuestion, len(questions))
	for _, q := range questions {
		byID[q.QuestionID] = q
	}

	supplied := make(map[string]acp.DecisionAnswer, len(answers))
	for _, a := range answers {
		q, ok := byID[a.QuestionID]
		if !ok {
			return nil, fmt.Errorf("answer references unknown question %q", a.QuestionID)
		}
		if _, dup := supplied[a.QuestionID]; dup {
			return nil, fmt.Errorf("question %q answered more than once", a.QuestionID)
		}
		if a.Outcome == nil {
			return nil, fmt.Errorf("answer for question %q has no outcome", a.QuestionID)
		}
		if err := validateOutcome(q, a.Outcome); err != nil {
			return nil, err
		}
		supplied[a.QuestionID] = a
	}

	out := make([]acp.DecisionAnswer, len(questions))
	for i, q := range questions {
		if a, ok := supplied[q.QuestionID]; ok {
			out[i] = a
			continue
		}
		out[i] = acp.DecisionAnswer{QuestionID: q.QuestionID, Outcome: acp.DecisionOutcomeCancelled{}}
	}
	return out, nil
}

// validateOutcome checks one answer's outcome against the question it answers.
// The type switch is CLOSED on purpose — its default rejects rather than waves
// through — because this is the seam untrusted input enters by (a peer's answer
// off the daemon wire, via internal/daemon's decision.answer op or a client's
// session/request_decision response). A pointer to an outcome variant,
// or a third-party implementation of the acp.DecisionOutcome interface, matches
// no case here; letting one past would skip every check below and then fall out
// of the tool's describeOutcome default, silently losing the id/label echo the
// model reasons about. Unknown means reject.
//
// It also enforces the question's OWN affordances: a client must not answer
// with free text (or with "let's chat") on a question whose model explicitly
// set allow_free_text/allow_chat to false. Those flags default to true, so
// switching one off is a deliberate act — honoring it is the difference between
// an escape hatch the agent offered and one a client invented.
func validateOutcome(q acp.DecisionQuestion, outcome acp.DecisionOutcome) error {
	switch o := outcome.(type) {
	case acp.DecisionOutcomeSelected:
		if !hasOption(q, o.OptionID) {
			return fmt.Errorf("question %q has no option %q", q.QuestionID, o.OptionID)
		}
	case acp.DecisionOutcomeText:
		if !q.AllowFreeText {
			return fmt.Errorf("question %q does not offer a free-text answer", q.QuestionID)
		}
	case acp.DecisionOutcomeChat:
		if !q.AllowChat {
			return fmt.Errorf("question %q does not offer the chat escape hatch", q.QuestionID)
		}
	case acp.DecisionOutcomeCancelled:
		// Always allowed: it is what an unanswered question normalizes to, and
		// what a client sends to withdraw from the whole request.
	default:
		return fmt.Errorf("answer for question %q has an unsupported outcome of type %T", q.QuestionID, outcome)
	}
	return nil
}

// hasOption reports whether q offers optionID.
func hasOption(q acp.DecisionQuestion, optionID string) bool {
	for _, o := range q.Options {
		if o.OptionID == optionID {
			return true
		}
	}
	return false
}
