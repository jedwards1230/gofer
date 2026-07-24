package decision_test

// stream_test.go covers the CLIENT side of the decision transport: the
// projection a daemon-backed consumer subscribes to. Its contract is
// deliberately the same as the Gate's where a consumer can tell the difference
// (replay on subscribe, drop-on-full delivery, a closed channel meaning "this
// stream is over") and deliberately different where the client's position
// requires it — chiefly that a resolution is published even for a request this
// stream never saw open.

import (
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/decision"
)

// streamRequest builds a Request with one two-option question, matching the
// shape the wire actually carries.
func streamRequest(sessionID, id string) decision.Request {
	return decision.Request{
		ID:        id,
		SessionID: sessionID,
		Questions: decision.AssignIDs([]acp.DecisionQuestion{{
			Title:    "Migration strategy",
			Question: "Which approach should I take?",
			Options: []acp.DecisionOption{
				{Label: "In-place ALTER"},
				{Label: "Shadow table + backfill", Recommended: true},
			},
			AllowFreeText: true,
			AllowChat:     true,
		}}),
	}
}

// recvUpdate reads one update off sub or fails.
func recvUpdate(t *testing.T, sub *decision.Subscription) decision.Update {
	t.Helper()
	select {
	case u, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription closed while waiting for an update")
		}
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an update")
		return decision.Update{}
	}
}

// expectNoUpdate asserts nothing further arrives promptly.
func expectNoUpdate(t *testing.T, sub *decision.Subscription) {
	t.Helper()
	select {
	case u, ok := <-sub.C:
		if ok {
			t.Fatalf("unexpected update %v/%s", u.Kind, u.Request.ID)
		}
		t.Fatal("subscription closed unexpectedly")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStreamPublishesAppliedUpdates(t *testing.T) {
	s := decision.NewStream()
	sub := s.Subscribe(4)
	defer sub.Close()

	req := streamRequest("sess-1", "dec-1")
	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: req})

	u := recvUpdate(t, sub)
	if u.Kind != decision.UpdateRequested || u.Request.ID != "dec-1" {
		t.Fatalf("update = %v/%s, want requested/dec-1", u.Kind, u.Request.ID)
	}
	if len(u.Request.Questions) != 1 || u.Request.Questions[0].QuestionID != "q1" {
		t.Fatalf("questions = %+v, want the applied question verbatim", u.Request.Questions)
	}

	s.Apply(decision.Update{Kind: decision.UpdateResolved, Request: decision.Request{ID: "dec-1", SessionID: "sess-1"}})
	if r := recvUpdate(t, sub); r.Kind != decision.UpdateResolved || r.Request.ID != "dec-1" {
		t.Fatalf("update = %v/%s, want resolved/dec-1", r.Kind, r.Request.ID)
	}
	if open := s.Open(); len(open) != 0 {
		t.Fatalf("Open() = %+v after the resolution, want empty", open)
	}
}

func TestStreamReplaysOpenRequestsToALateSubscriber(t *testing.T) {
	s := decision.NewStream()
	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: streamRequest("sess-1", "dec-1")})
	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: streamRequest("sess-1", "dec-2")})
	s.Apply(decision.Update{Kind: decision.UpdateResolved, Request: decision.Request{ID: "dec-1", SessionID: "sess-1"}})

	// A subscriber arriving now sees only what is still open, in arrival order.
	sub := s.Subscribe(4)
	defer sub.Close()

	u := recvUpdate(t, sub)
	if u.Kind != decision.UpdateRequested || u.Request.ID != "dec-2" {
		t.Fatalf("replayed %v/%s, want requested/dec-2 (dec-1 already resolved)", u.Kind, u.Request.ID)
	}
	expectNoUpdate(t, sub)
}

// TestStreamFoldsADuplicateRequest: the daemon replays an open request on every
// attach, so a client that already saw it live receives it twice. The second
// must refresh the entry in place — not stack a second open request that would
// replay forever.
func TestStreamFoldsADuplicateRequest(t *testing.T) {
	s := decision.NewStream()
	req := streamRequest("sess-1", "dec-1")
	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: req})
	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: req})

	if open := s.Open(); len(open) != 1 {
		t.Fatalf("Open() = %d entries after a duplicate, want 1", len(open))
	}
	sub := s.Subscribe(4)
	defer sub.Close()
	if u := recvUpdate(t, sub); u.Request.ID != "dec-1" {
		t.Fatalf("replayed %s, want dec-1", u.Request.ID)
	}
	expectNoUpdate(t, sub)
}

// TestStreamPublishesAnUnknownResolution: a resolution for a request this stream
// never saw open is still published. A Stream is a LOSSY projection of a gate
// (an overflowing subscriber drops updates), so swallowing the one message that
// clears a prompt is the failure that would leave a question on screen forever.
func TestStreamPublishesAnUnknownResolution(t *testing.T) {
	s := decision.NewStream()
	sub := s.Subscribe(4)
	defer sub.Close()

	s.Apply(decision.Update{Kind: decision.UpdateResolved, Request: decision.Request{ID: "dec-9", SessionID: "sess-1"}})
	if u := recvUpdate(t, sub); u.Kind != decision.UpdateResolved || u.Request.ID != "dec-9" {
		t.Fatalf("update = %v/%s, want resolved/dec-9", u.Kind, u.Request.ID)
	}
}

// TestStreamCloseResolvesOpenRequestsThenClosesSubscriptions: the connection
// going away must clear a client's prompt (a resolution per open request) AND
// end its pump (a closed channel), in that order.
func TestStreamCloseResolvesOpenRequestsThenClosesSubscriptions(t *testing.T) {
	s := decision.NewStream()
	sub := s.Subscribe(4)
	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: streamRequest("sess-1", "dec-1")})
	if u := recvUpdate(t, sub); u.Kind != decision.UpdateRequested {
		t.Fatalf("update = %v, want requested", u.Kind)
	}

	s.Close()

	if u := recvUpdate(t, sub); u.Kind != decision.UpdateResolved || u.Request.ID != "dec-1" {
		t.Fatalf("update = %v/%s, want resolved/dec-1 on close", u.Kind, u.Request.ID)
	}
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("subscription delivered an update after close, want a closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscription stayed open after Close — a consumer's pump would park forever")
	}

	// Idempotent, and inert afterwards.
	s.Close()
	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: streamRequest("sess-1", "dec-2")})
	if open := s.Open(); len(open) != 0 {
		t.Fatalf("Open() = %+v after close, want empty — a closed stream applies nothing", open)
	}
	sub.Close() // idempotent: closing an already-closed subscription must not panic
}

// TestStreamSubscribeAfterCloseIsClosed: a consumer that subscribes to an
// already-dead stream learns it through the same closed-channel path as one
// whose stream died under it, not through a second error path.
func TestStreamSubscribeAfterCloseIsClosed(t *testing.T) {
	s := decision.NewStream()
	s.Close()

	sub := s.Subscribe(4)
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("a subscription to a closed stream delivered an update")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a subscription to a closed stream is open — the consumer's pump would park forever")
	}
	sub.Close()
}

// TestStreamDropsOnFullBufferRatherThanBlocking: the Apply path runs on a wire
// demuxer that also drains the connection's control plane, so a wedged consumer
// must cost that consumer a dropped update and nothing else.
func TestStreamDropsOnFullBufferRatherThanBlocking(t *testing.T) {
	s := decision.NewStream()
	sub := s.Subscribe(1)
	defer sub.Close()

	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: streamRequest("sess-1", "dec-1")})
	// Nothing is reading: this one has nowhere to go.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: streamRequest("sess-1", "dec-2")})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Apply blocked on a full subscriber buffer — it must drop instead")
	}

	if got := sub.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
	// The dropped update is still in the open set, so the next subscriber sees
	// both — losing a delivery must not lose the state.
	if open := s.Open(); len(open) != 2 {
		t.Fatalf("Open() = %d entries, want 2 (a dropped delivery is not a dropped request)", len(open))
	}
}

// TestStreamSubscribeNegativeBufferClamps mirrors the gate's own clamp: a
// negative buffer is 0, and the replay still fits.
func TestStreamSubscribeNegativeBufferClamps(t *testing.T) {
	s := decision.NewStream()
	s.Apply(decision.Update{Kind: decision.UpdateRequested, Request: streamRequest("sess-1", "dec-1")})

	sub := s.Subscribe(-5)
	defer sub.Close()
	if u := recvUpdate(t, sub); u.Request.ID != "dec-1" {
		t.Fatalf("replayed %s, want dec-1", u.Request.ID)
	}
}
