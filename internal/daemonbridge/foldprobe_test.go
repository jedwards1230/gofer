package daemonbridge_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// foldProbe brackets a session/load-triggering call with a snapshot of the
// session's folded history taken immediately BEFORE it and, on the failure
// path, again AFTER — so a history-replay assertion that fails can say what
// the daemon had available to replay, not just which events arrived.
//
// It is the daemonbridge counterpart of internal/daemon's foldProbe (that
// one lives in package daemon_test and is unreachable from here); see its doc
// for the full rationale. The short version:
//
//   - BEFORE already short => the fold did not contain the turn at read time.
//     The replay was short at the SOURCE; no event went missing downstream,
//     because there was never an event to lose.
//   - BEFORE complete      => the daemon necessarily folded a complete history
//     (the fold is append-only for a live session), so a missing event went
//     astray in DELIVERY.
//
// Here the load is not issued directly by the test: [daemonbridge.Supervisor]'s
// Subscribe triggers session/load internally for a session it has no
// reconstruction state for. The BEFORE snapshot is therefore taken immediately
// before that Subscribe, which still strictly precedes the daemon's fold.
//
// The window this exists to detect: a turn's assistant/tool entries are
// journaled ASYNCHRONOUSLY by the SDK runner's consume goroutine, off the same
// broker that delivers turn.finished to clients. A test that advances on
// turn.finished — as this package's event drains do, since
// [daemonbridge.Supervisor.Send] is fire-and-forget and never waits for the
// session/prompt response — can therefore reach its next Subscribe before the
// turn it just watched finish has been appended.
//
// Measurement only: it reads through the public
// [supervisor.Supervisor.History] accessor, takes no locks, adds no sleeps or
// retries, and never changes what a test asserts.
type foldProbe struct {
	t      *testing.T
	sup    *supervisor.Supervisor
	sid    string
	before string
}

// awaitFoldComplete blocks until sid's folded history is COMPLETE — it holds
// wantBlocks content blocks, exactly the ones the caller is about to assert are
// replayed — so the session/load the caller's Subscribe triggers reads a fold
// that is already whole.
//
// It is the daemonbridge counterpart of internal/daemon's awaitFoldComplete
// (that one lives in package daemon_test and is unreachable from here); see its
// doc for the full rationale. The short version:
//
//   - Asserting a replay without synchronising against the asynchronous
//     journaling window (see [foldProbe]'s doc) asserts a coincidence — that
//     the fold happened to be complete when the daemon read it. That is a claim
//     about TIMING.
//   - The journal is APPEND-ONLY, so a fold observed complete STAYS complete.
//     Observing completeness once therefore makes every later read — including
//     the daemon's own, inside the load this call precedes — necessarily
//     complete. The replay assertion afterwards is then about the SYSTEM.
//
// Not a sleep, a retry, or a poll, and not to be "simplified" into one: it
// blocks on [supervisor.Supervisor.WatchRoster] and advances on the session
// reporting [supervisor.StatusNeedsInput], which the pump publishes only after
// Session.Prompt returns — i.e. after the runner's journaling barrier. The fold
// is then read exactly ONCE and required to be complete, so a wrong inference
// fails loudly here rather than flaking at the replay assertion.
//
// Call it only after the turn being replayed has been observed to finish.
func awaitFoldComplete(t *testing.T, sup *supervisor.Supervisor, sid string, wantBlocks int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	roster, err := sup.WatchRoster(ctx)
	if err != nil {
		t.Fatalf("WatchRoster: %v", err)
	}
	for {
		select {
		case snap, ok := <-roster:
			if !ok {
				t.Fatalf("roster watch closed before session %s reported needs-input\n  fold: %s",
					sid, describeFold(t, sup, sid))
			}
			if !needsInput(snap, sid) {
				continue
			}
			if got := foldBlocks(t, sup, sid); got != wantBlocks {
				t.Fatalf("session %s reported needs-input holding %d folded content block(s), want %d"+
					"\n  fold: %s\n  (needs-input means the runner's journaling barrier has passed, so the"+
					"\n   fold must already be whole here. See awaitFoldComplete's doc.)",
					sid, got, wantBlocks, describeFold(t, sup, sid))
			}
			return
		case <-ctx.Done():
			t.Fatalf("timed out after %s waiting for session %s to report needs-input\n  fold: %s",
				defaultWait, sid, describeFold(t, sup, sid))
		}
	}
}

// needsInput reports whether snap holds sid with an idle pump and an empty
// queue — the transition [awaitFoldComplete] waits on.
func needsInput(snap []supervisor.SessionInfo, sid string) bool {
	for _, s := range snap {
		if s.ID == sid {
			return s.Status == supervisor.StatusNeedsInput
		}
	}
	return false
}

// foldBlocks counts the content blocks across a session's folded history — one
// per started/finished pair a history replay emits.
func foldBlocks(t *testing.T, sup *supervisor.Supervisor, sid string) int {
	t.Helper()
	msgs, err := sup.History(context.Background(), sid)
	if err != nil {
		t.Fatalf("History(%s): %v", sid, err)
	}
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

// newFoldProbe captures the pre-load fold snapshot. Call it immediately before
// the Subscribe (or explicit load) whose replay is being asserted.
func newFoldProbe(t *testing.T, sup *supervisor.Supervisor, sid string) *foldProbe {
	t.Helper()
	return &foldProbe{t: t, sup: sup, sid: sid, before: describeFold(t, sup, sid)}
}

// diagnosis captures the post-load fold snapshot and renders both alongside the
// events that actually arrived, as a failure-message suffix. Call it only on
// the failure path.
func (p *foldProbe) diagnosis(got []string) string {
	p.t.Helper()
	return fmt.Sprintf(
		"\n  fold BEFORE load: %s\n  fold AFTER  load: %s\n  events received : %v"+
			"\n  (a short BEFORE means the history was incomplete AT READ TIME — the replay was"+
			"\n   short at the source; a complete BEFORE means the events went astray in delivery."+
			"\n   See foldProbe's doc.)",
		p.before, describeFold(p.t, p.sup, p.sid), got)
}

// eventKinds names each event's kind, for reporting which frames arrived
// alongside a [foldProbe] snapshot.
func eventKinds(evs []event.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind())
	}
	return out
}

// describeFold renders a session's current folded history as a compact,
// deterministic shape — message count, then each message's role and content
// block kinds — suitable for a failure message.
func describeFold(t *testing.T, sup *supervisor.Supervisor, sid string) string {
	t.Helper()
	msgs, err := sup.History(context.Background(), sid)
	if err != nil {
		return fmt.Sprintf("<unavailable: %v>", err)
	}
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		kinds := make([]string, 0, len(m.Content))
		for _, b := range m.Content {
			kinds = append(kinds, blockKind(b))
		}
		parts = append(parts, fmt.Sprintf("%s[%s]", m.Role, strings.Join(kinds, " ")))
	}
	return fmt.Sprintf("%d message(s) %s", len(msgs), strings.Join(parts, " "))
}

// blockKind names one content block's type for [describeFold].
func blockKind(b provider.ContentBlock) string {
	switch b.Type {
	case provider.BlockText:
		return "text"
	case provider.BlockReasoning:
		return "reasoning"
	case provider.BlockToolUse:
		return "tool_use"
	case provider.BlockToolResult:
		return "tool_result"
	case provider.BlockImage:
		return "image"
	default:
		return "unknown"
	}
}
