package wirestream

// kindcoverage_internal_test.go is the regression pin for handleGoferEvent's
// losslessness — the property reconstruct.go's package doc claims and the reason
// that method delegates to the SDK's [event.Unmarshal] instead of carrying a
// decode switch of its own.
//
// The failure mode it exists to prevent is silent by construction: a kind this
// core never decodes looks EXACTLY like a kind the producer never sends. Nothing
// errors, nothing logs, no client complains — the frame simply is not there. The
// hand-rolled 16-case table this file post-dates had drifted four items behind
// the union without anyone noticing: `plan` and `session.config` had no case at
// all (dropped whole, in-turn as well as out-of-turn, for every client under
// --workers), and turn.finished's ContextWindow and tool.call.finished's Edits
// were shed field-wise because both are set on the built event AFTER
// construction, so no event.New* signature could carry them. The first of those
// was user-visible in a way nobody traced back here: ACP's projection gates its
// usage_update on ContextWindow > 0, so a pure-ACP peer attached through a
// router received no usage update at all.
//
// See also reconstruct_internal_test.go's TestHandleNotificationReplaysGoferEventKinds,
// which asserts the same round trip on a hand-picked corpus. This file differs
// in the two ways that make it a coverage pin rather than a sample: it carries
// ONE entry per kind in the union, and every payload field of every entry is set
// to a distinctive non-zero value — a zero field cannot demonstrate that it was
// dropped, because "dropped" and "zero" decode identically.

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// The three kinds whose union types carry fields the SDK sets on the BUILT event
// rather than through a constructor. They are broken out as builders because Go
// has no way to assign a field on a call result, and they are the exact fields
// the replaced dispatch table could not carry — which is why each is populated
// here rather than left zero.

// fullTurnFinished builds a turn.finished with every payload field set,
// INCLUDING ContextWindow (post-construction; see [event.TurnFinished]).
func fullTurnFinished(sid string) event.TurnFinished {
	e := event.NewTurnFinishedCost(sid, "end_turn",
		provider.Usage{
			InputTokens:      1301,
			OutputTokens:     412,
			CacheReadTokens:  77,
			CacheWriteTokens: 19,
			Raw:              map[string]int{"reasoning_tokens": 64},
		},
		&provider.Cost{USD: 0.0421, InputUSD: 0.031, OutputUSD: 0.0091, CacheReadUSD: 0.0015, CacheWriteUSD: 0.0005},
	)
	e.ContextWindow = 192_512
	return e
}

// fullToolCallStarted builds a tool.call.started with Agent set
// (post-construction; see [event.ToolCallStarted]).
func fullToolCallStarted(sid string) event.ToolCallStarted {
	e := event.NewToolCallStarted(sid, "call-77", "edit_file", json.RawMessage(`{"path":"main.go","old":"a"}`))
	e.Agent = "researcher"
	return e
}

// fullToolCallDelta builds a tool.call.delta with Agent set.
func fullToolCallDelta(sid string) event.ToolCallDelta {
	e := event.NewToolCallDelta(sid, "call-77", `{"path":"ma`)
	e.Agent = "researcher"
	return e
}

// fullToolCallFinished builds a tool.call.finished with every payload field set,
// INCLUDING Edits and Agent (both post-construction; see
// [event.ToolCallFinished]).
func fullToolCallFinished(sid string) event.ToolCallFinished {
	e := event.NewToolCallFinishedSpill(sid, "call-77",
		json.RawMessage(`{"path":"main.go","old":"a"}`),
		"bounded excerpt of the tool output",
		true,
		[]string{"lint: unused variable x", "vet: possible nil dereference at line 42"},
		"sessions/proj/sess-kinds/calls/call-77.log", 41_231,
		"9f2c1e5b7a3d4f6089c2e1b5a7d3f46089c2e1b5a7d3f46089c2e1b5a7d3f460",
	)
	e.Edits = []event.FileEdit{
		{Path: "main.go", OldText: "a", NewText: "b"},
		{Path: "internal/x/y.go", NewText: "package y\n"},
	}
	e.Agent = "researcher"
	return e
}

// TestReconstructCarriesEveryEventKind pins that EVERY event kind survives the
// gofer/event wire → broker round trip with every payload field intact.
//
// # What it is for
//
// handleGoferEvent is the SOLE decode path for a worker's frames under
// --workers: whatever it does not reconstruct reaches no client in that mode, at
// all, and it does so without an error anywhere. That is why this is a table of
// the whole union rather than a sample — the interesting cases are precisely the
// ones nobody thought to sample. Each kind is named through its `event.Kind*`
// const, so a rename in the SDK breaks THIS BUILD rather than silently skipping
// a row; and each entry's payload is populated field by field with distinctive
// non-zero values, because a field left zero passes this test whether it was
// carried or dropped.
//
// # Exhaustiveness is now structural
//
// The point of the table is NOT that someone must extend it whenever the SDK
// grows a kind. Since handleGoferEvent delegates to [event.Unmarshal] — the
// maintained inverse of the MarshalJSON that wrote the bytes — a kind added to
// the union is carried here the day it exists, with no change to this package.
// The table is the pin against the OTHER direction: anyone reintroducing per-kind
// logic in handleGoferEvent (a switch, a local mirror of the union's payload
// fields, a "we only care about these kinds" filter) has to make all of it pass
// first, which is exactly what the deleted 16-case table could not do.
//
// # What is compared, and why seq/time are excluded
//
// The source event is marshalled with its OWN MarshalJSON — byte-for-byte what
// the daemon's broadcastGoferEvent puts on the wire — and those bytes are fed to
// handleGoferEvent. The event that lands on the session's broker is then compared
// with reflect.DeepEqual against what [event.Unmarshal] makes of the same bytes:
// concrete type and every field, not just the kind.
//
// Only seq and time are excluded, and they are excluded by NORMALIZATION rather
// than by a field-by-field comparison that could quietly skip more than it says.
// [event.Broker.Publish] reassigns both on every publish (see reconstruct.go's
// package doc for why that is by design, not a fidelity gap), so the published
// event is re-encoded and its seq/time replaced by the source envelope's before
// the final decode. Both sides of the comparison then come out of Unmarshal with
// identical meta, and any surviving difference is a payload field this core lost.
func TestReconstructCarriesEveryEventKind(t *testing.T) {
	const sid = "sess-kinds"

	// One entry per kind in the SDK's union, minus permission.requested /
	// permission.resolved — those are excluded from gofer/event BY CONTRACT (see
	// methodGoferEvent's doc) and get their own assertion in
	// TestReconstructDropsPermissionKindsOnGoferEvent below.
	table := []struct {
		kind string
		ev   event.Event
	}{
		{event.KindSessionCreated, event.NewSessionCreated(sid)},
		{event.KindSessionResumed, event.NewSessionResumed(sid)},
		{event.KindSessionForked, event.NewSessionForked(sid, "entry-31", "before-the-refactor")},
		{event.KindSessionCompacted, event.NewSessionCompacted(sid, "entry-42", 17, "claude-sonnet-5",
			provider.Usage{InputTokens: 903, OutputTokens: 121, CacheReadTokens: 41, CacheWriteTokens: 11,
				Raw: map[string]int{"reasoning_tokens": 12}},
			"a summary of the compacted turns")},
		{event.KindSessionKilled, event.NewSessionKilled(sid)},
		{event.KindSessionArchived, event.NewSessionArchived(sid)},
		{event.KindSessionSpawned, event.NewSessionSpawned(sid, "child-9", "researcher", 2)},
		{event.KindSessionInfo, event.NewSessionInfoUpdated(sid, "refactor the wire decoder")},
		{event.KindSessionConfig, event.NewConfigOptionsUpdated(sid, []event.ConfigOption{{
			ID:            "model",
			Name:          "Model",
			Description:   "the model this session runs",
			Category:      "session",
			Kind:          event.ConfigOptionSelect,
			SelectedValue: "claude-sonnet-5",
			Values: []event.ConfigSelectValue{
				{Value: "claude-sonnet-5", Name: "Sonnet 5", Description: "balanced"},
				{Value: "claude-opus-5", Name: "Opus 5", Description: "deepest"},
			},
		}, {
			ID:      "thinking",
			Name:    "Thinking",
			Kind:    event.ConfigOptionBoolean,
			Enabled: true,
		}})},
		{event.KindPlan, event.NewPlanUpdated(sid, []event.PlanEntry{
			{Content: "read the SDK's unmarshal", Priority: "high", Status: "completed"},
			{Content: "delete the hand-rolled switch", Priority: "medium", Status: "in_progress"},
		})},
		{event.KindSessionError, event.NewSessionError(sid, "the provider hung up", true)},
		{event.KindTurnStarted, event.NewTurnStarted(sid)},
		{event.KindTurnFinished, fullTurnFinished(sid)},
		{event.KindMessageStarted, event.NewMessageStarted(sid, event.MessageReasoning)},
		{event.KindMessageDelta, event.NewMessageDelta(sid, event.MessageText, "a fragment of streamed text")},
		{event.KindMessageFinished, event.NewMessageFinishedMeta(sid, event.MessageReasoning,
			"the settled reasoning content", map[string]string{"anthropic.signature": "sig-abc123"})},
		{event.KindToolCallStarted, fullToolCallStarted(sid)},
		{event.KindToolCallDelta, fullToolCallDelta(sid)},
		{event.KindToolCallFinished, fullToolCallFinished(sid)},
	}

	seen := make(map[string]bool, len(table))
	for _, tc := range table {
		// A copy-paste in the table would otherwise assert one kind twice and
		// leave another wholly uncovered, with the row count still looking right.
		if tc.ev.Kind() != tc.kind {
			t.Fatalf("table row %q carries a %q event; the kind const and the value must agree", tc.kind, tc.ev.Kind())
		}
		if seen[tc.kind] {
			t.Fatalf("table lists %q twice", tc.kind)
		}
		seen[tc.kind] = true
	}
	if seen[event.KindPermissionRequested] || seen[event.KindPermissionResolved] {
		t.Fatal("permission.* is excluded from gofer/event by contract and must not be in the round-trip table")
	}

	for _, tc := range table {
		t.Run(tc.kind, func(t *testing.T) {
			// The sink is installed so a kind that reaches the broker but NOT the
			// forwarding seam (or the reverse) fails here rather than in the
			// router, where it would present as a client missing frames.
			var sunk []event.Event
			r := newSinkTestReconstructor(func(_ string, _ json.RawMessage, ev event.Event) {
				sunk = append(sunk, ev)
			})
			r.RegisterFresh(sid)
			sub, err := r.Subscribe(context.Background(), sid)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Close()

			// The source event's own MarshalJSON envelope, verbatim — exactly the
			// params the daemon's broadcastGoferEvent writes.
			raw, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatalf("marshal source %T: %v", tc.ev, err)
			}
			r.handleNotification(goferEventFrame(string(raw)))

			var got event.Event
			select {
			case got = <-sub.C:
			case <-time.After(time.Second):
				t.Fatalf("nothing published for %q: the wire→broker round trip dropped the kind entirely", tc.kind)
			}

			want, err := event.Unmarshal(raw)
			if err != nil {
				t.Fatalf("event.Unmarshal of the source envelope: %v", err)
			}
			if normalized := matchSeqTime(t, got, raw); !reflect.DeepEqual(normalized, want) {
				t.Errorf("replayed %q lost or altered a payload field\n got: %#v\nwant: %#v", tc.kind, normalized, want)
			}

			if len(sunk) != 1 {
				t.Fatalf("sink invoked %d times for %q, want exactly 1", len(sunk), tc.kind)
			}
			if sunk[0].Kind() != tc.kind {
				t.Errorf("sink saw a %q event, want %q", sunk[0].Kind(), tc.kind)
			}
		})
	}
}

// matchSeqTime re-decodes got with the seq/time of the SOURCE envelope in place
// of the ones [event.Broker.Publish] assigned, so a reflect.DeepEqual against
// event.Unmarshal(src) compares every payload field for real while the two
// fields a publish legitimately rewrites are equal by construction.
//
// Why a re-decode rather than reading got.Seq()/got.Time() and comparing the
// rest: an event's meta is unexported, so there is no way to build a comparable
// value with a chosen seq/time from outside the SDK — and the broker stamps
// time.Now(), which carries a monotonic reading that never compares DeepEqual to
// a time parsed from the wire. Round-tripping got through its own MarshalJSON
// puts both sides on the same footing (wall clock, no monotonic, meta populated
// by the same decoder), which is what makes the comparison total instead of a
// hand-maintained list of fields to check.
func matchSeqTime(t *testing.T, got event.Event, src json.RawMessage) event.Event {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal the published %T: %v", got, err)
	}
	var gm, sm map[string]json.RawMessage
	if err := json.Unmarshal(raw, &gm); err != nil {
		t.Fatalf("decode the published envelope: %v", err)
	}
	if err := json.Unmarshal(src, &sm); err != nil {
		t.Fatalf("decode the source envelope: %v", err)
	}
	// "time" is omitempty on the wire, so an absent key is meaningful (a zero
	// publish time) and must be carried over as an absence, not as a zero value.
	for _, k := range []string{"seq", "time"} {
		if v, ok := sm[k]; ok {
			gm[k] = v
		} else {
			delete(gm, k)
		}
	}
	restamped, err := json.Marshal(gm)
	if err != nil {
		t.Fatalf("re-encode the normalized envelope: %v", err)
	}
	ev, err := event.Unmarshal(restamped)
	if err != nil {
		t.Fatalf("event.Unmarshal of the normalized envelope: %v", err)
	}
	return ev
}

// TestReconstructDropsPermissionKindsOnGoferEvent is the other half of the
// coverage claim: permission.requested and permission.resolved are the two kinds
// the table above deliberately omits, and omitting them is only defensible if
// their absence is ENFORCED rather than incidental.
//
// It is enforced, and it has to be explicitly. [event.Unmarshal] decodes both
// kinds perfectly well, so the exclusion cannot be a side effect of "the decoder
// doesn't know them" the way it was under the old dispatch table — handleGoferEvent
// checks the discriminator and returns first. permission.* travels the dedicated
// gofer/permission_* methods (see methodGoferEvent's doc), which
// handleNotification dispatches separately; a frame that also arrived on
// gofer/event would be delivered TWICE, once on each path.
//
// Both fan-outs are asserted, not just the broker: a frame forwarded to the sink
// but not published (or the reverse) would give the router and this core
// different ideas of what the session's stream contains.
func TestReconstructDropsPermissionKindsOnGoferEvent(t *testing.T) {
	const sid = "sess-perm"
	for _, tc := range []struct {
		kind string
		raw  string
	}{
		{event.KindPermissionRequested, `{"type":"permission.requested","session_id":"sess-perm","id":"perm-1","tool":"bash","spec":{"cmd":"rm -rf /"},"trace":["no rule"]}`},
		{event.KindPermissionResolved, `{"type":"permission.resolved","session_id":"sess-perm","id":"perm-1","verdict":"deny","rule":"deny bash rm"}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var sinkCalls int
			r := newSinkTestReconstructor(func(string, json.RawMessage, event.Event) { sinkCalls++ })
			r.RegisterFresh(sid)
			sub, err := r.Subscribe(context.Background(), sid)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Close()

			r.handleNotification(goferEventFrame(tc.raw))

			select {
			case ev := <-sub.C:
				t.Errorf("published %+v; %q on gofer/event must be dropped (it rides gofer/permission_* instead)", ev, tc.kind)
			case <-time.After(50 * time.Millisecond):
			}
			if sinkCalls != 0 {
				t.Errorf("sink invoked %d times for a %q gofer/event frame, want 0", sinkCalls, tc.kind)
			}
		})
	}
}

// TestReconstructDropsUnknownKind pins the protocol-drift tolerance the
// delegation to [event.Unmarshal] inherits: a frame from a NEWER gofer, carrying
// a kind this binary's SDK has never heard of, is dropped quietly — no publish,
// no sink push, no panic, and no crashed replay for the kinds around it.
//
// Dropping it from the sink too is the load-bearing half. The router forwards
// what the sink hands it; forwarding a frame this core could not decode would
// leave the router's fan-out and this core's broker disagreeing about what the
// session's stream contains, which is a worse failure than the missing frame.
func TestReconstructDropsUnknownKind(t *testing.T) {
	const sid = "sess-drifted"
	var sinkCalls int
	r := newSinkTestReconstructor(func(string, json.RawMessage, event.Event) { sinkCalls++ })
	r.RegisterFresh(sid)
	sub, err := r.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	r.handleNotification(goferEventFrame(`{"type":"session.teleported","session_id":"sess-drifted","destination":"mars"}`))

	select {
	case ev := <-sub.C:
		t.Errorf("published %+v for an unknown kind; a future producer's frame must be dropped, not guessed at", ev)
	case <-time.After(50 * time.Millisecond):
	}
	if sinkCalls != 0 {
		t.Errorf("sink invoked %d times for an unknown kind, want 0", sinkCalls)
	}

	// The core is still live afterwards: drift tolerance means skip-and-continue,
	// not a wedged replay.
	r.handleNotification(goferEventFrame(`{"type":"turn.started","session_id":"sess-drifted"}`))
	select {
	case ev := <-sub.C:
		if _, ok := ev.(event.TurnStarted); !ok {
			t.Errorf("published %T after the unknown kind, want event.TurnStarted", ev)
		}
	case <-time.After(time.Second):
		t.Error("replay stopped after an unknown kind; drift must be skip-and-continue")
	}
}
