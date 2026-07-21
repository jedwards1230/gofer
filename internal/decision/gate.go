package decision

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// Gate is one session's registry of outstanding structured-decision requests —
// the decision-side analogue of the SDK's loop.Gate for permissions. The
// `ask_user` tool opens a request with [Gate.Request] (blocking the agent's
// turn on the calling goroutine); clients watch the open set with
// [Gate.Subscribe] and resolve a request with [Gate.Answer].
//
// Like loop.Gate, Request spawns no goroutine: it blocks the caller on a select
// between a buffered answer channel and ctx.Done, so an interrupted turn
// releases the waiter and leaves nothing running. Unlike loop.Gate there is no
// "answer arrived before the waiter registered" window to stash: the request id
// is minted inside Request, under the same lock that publishes it, so no client
// can name a request before its waiter exists.
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
	// under mu, exactly once (whichever of Answer/ctx-cancel gets there
	// first).
	open  map[string]*pending
	order []string
	subs  map[*Subscription]struct{}
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
		subs:      make(map[*Subscription]struct{}),
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
// The returned answers are normalized by [Gate.Answer]: exactly one per
// question, in question order, with anything the client left out filled in as
// [acp.DecisionOutcomeCancelled]. On ctx cancellation the request is dropped
// from the open set and an [UpdateResolved] is published, so a client
// rendering the prompt clears it rather than answering a request that no
// longer has a waiter.
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
	if len(g.subs) == 0 {
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
	case <-ctx.Done():
		g.drop(p.req.ID)
		return nil, ctx.Err()
	}
}

// Answer resolves the outstanding request requestID with the client's answers.
// It returns [ErrUnknownRequest] for an id that is not open, and a descriptive
// error for an answer that does not fit the request: an unknown question id, a
// second answer for a question already answered, a selected outcome naming an
// option the question does not offer, or an answer with no outcome at all.
// A rejected call leaves the request open, so the client can correct and retry.
//
// Validating here rather than in [Gate.Request]'s caller is deliberate: this is
// the seam untrusted-ish input (a peer's answer over the daemon wire, in the
// follow-up PR) enters through, and the tool downstream must be able to trust
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
	g.mu.Unlock()

	// Buffered, and the request was just removed under mu, so exactly one
	// Answer can ever reach this send for a given id and it never blocks —
	// not even when the waiter already left on ctx.
	p.answers <- normalized
	return nil
}

// Open returns a snapshot of the currently-open requests, in the order they
// were opened. Each Request shares its Questions slice with the live request;
// treat it as read-only. It is the recovery path for a client that missed an
// [UpdateRequested] (see [Subscription.Dropped]) and the replay source for a
// client attaching mid-flight.
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
// a prompt; [Gate.Open] is its recovery, and buffer should simply be large
// enough (a session has one outstanding decision at a time in practice).
func (g *Gate) Subscribe(buffer int) *Subscription {
	if buffer < 0 {
		buffer = 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	replay := g.openLocked()
	capacity := buffer
	if len(replay) > capacity {
		capacity = len(replay)
	}
	ch := make(chan Update, capacity)
	sub := &Subscription{C: ch, ch: ch, gate: g}
	for _, r := range replay {
		ch <- Update{Kind: UpdateRequested, Request: r} // fits: capacity >= len(replay)
	}
	g.subs[sub] = struct{}{}
	return sub
}

// drop removes requestID from the open set and publishes its resolution, if it
// is still open. It is the ctx-cancellation half of the "a request leaves the
// open set exactly once" invariant: whichever of drop and [Gate.Answer]
// acquires mu first wins, and the loser sees no entry and does nothing.
func (g *Gate) drop(requestID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.open[requestID]; !ok {
		return
	}
	g.removeLocked(requestID)
	g.publishLocked(Update{Kind: UpdateResolved, Request: Request{ID: requestID, SessionID: g.sessionID}})
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
// full buffer rather than blocking — see [Gate.Subscribe]. Callers hold g.mu,
// which is safe precisely because the send never blocks.
func (g *Gate) publishLocked(u Update) {
	for sub := range g.subs {
		select {
		case sub.ch <- u:
		default:
			sub.dropped.Add(1)
		}
	}
}

// Subscription is a receive stream of [Update]s from a [Gate]. Range over C to
// consume updates; call Close to unsubscribe. C is closed by Close and only by
// Close — a Gate outlives its session's turns, so there is no broker-side
// shutdown that could close it out from under a consumer.
type Subscription struct {
	// C receives updates. It is closed when the subscription is closed.
	C <-chan Update

	ch      chan Update
	gate    *Gate
	dropped atomic.Uint64
	once    sync.Once
}

// Dropped returns how many updates were discarded because this subscriber's
// buffer was full. A non-zero count means the client may be missing an open
// request and should reconcile against [Gate.Open].
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Close unsubscribes and closes C. It is idempotent and safe to call
// concurrently with delivery: the unsubscribe takes the gate's lock, which a
// concurrent publish also holds, so no send can race the close.
func (s *Subscription) Close() {
	s.gate.mu.Lock()
	delete(s.gate.subs, s)
	s.gate.mu.Unlock()
	s.once.Do(func() { close(s.ch) })
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
		if sel, ok := a.Outcome.(acp.DecisionOutcomeSelected); ok && !hasOption(q, sel.OptionID) {
			return nil, fmt.Errorf("question %q has no option %q", a.QuestionID, sel.OptionID)
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

// hasOption reports whether q offers optionID.
func hasOption(q acp.DecisionQuestion, optionID string) bool {
	for _, o := range q.Options {
		if o.OptionID == optionID {
			return true
		}
	}
	return false
}
