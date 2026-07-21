package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// foldProbe brackets a session/load with a snapshot of the session's folded
// history taken immediately BEFORE the load request and immediately AFTER its
// response, so a history-replay assertion that fails can say what the daemon
// had available to replay — not just which frames arrived.
//
// # Why a bracket, and why it is conclusive
//
// A test client cannot observe the daemon's own Fold() call directly. It can,
// however, sandwich it: the daemon reads the fold strictly between the moment
// the load request is written and the moment its response is read (see
// handleSessionLoad, which folds after Resume and replays before returning).
// So the two snapshots bound the daemon's read, and the pair discriminates the
// two competing explanations for a short replay:
//
//   - BEFORE is already short  => the fold did not yet contain the turn at read
//     time; the replay was short at the SOURCE. Nothing downstream lost a
//     frame — there was never a frame to lose.
//   - BEFORE is complete       => the daemon necessarily folded a complete
//     history (the fold only grows; see below), so any missing frame went
//     astray in DELIVERY.
//
// The fold is append-only for a live session — [session.Journal.Append] only
// ever extends the entry log, and Fold walks HEAD back to the root — so
// "BEFORE is complete" really does imply "the daemon's later read was complete".
// AFTER is recorded to show whether a short BEFORE had filled in by the time
// the response landed, which sizes the window.
//
// # Why the fold can lag a turn the client has already seen finish
//
// A turn's assistant/tool entries are journaled ASYNCHRONOUSLY: the SDK
// runner's consume goroutine subscribes to the session broker and appends each
// settled turn as it drains it, while the user's own message is appended
// synchronously by Prompt before the loop runs. turn.finished is published to
// every subscriber — including the daemon's session/prompt handler, which
// returns the RPC response on it — before consume has necessarily dequeued and
// appended anything. So a client that observes a turn finish (by the prompt
// response OR by the event stream) and immediately loads can legitimately read
// a fold holding only the user message. Runner.Prompt closes this window with
// its awaitJournaled barrier, but nothing on the session/load path waits for
// that barrier.
//
// This probe is measurement only: it reads through the same public
// [supervisor.Supervisor.History] accessor the daemon uses, takes no locks,
// adds no sleeps or retries, and never changes what the test asserts.
type foldProbe struct {
	t      *testing.T
	sup    *supervisor.Supervisor
	sid    string
	before string
}

// awaitFoldComplete blocks until sid's folded history is COMPLETE — it holds
// wantBlocks content blocks, exactly the ones the caller is about to assert are
// replayed — and only then returns, so the caller's session/load reads a fold
// that is already whole.
//
// # Why this strengthens the assertion rather than papering over a race
//
// A history-replay test asserts across an asynchronous boundary: the user
// message is appended synchronously by Prompt, while the turn's assistant and
// tool entries are appended later by the SDK runner's consume goroutine (see
// [foldProbe]'s doc for that window). A test that loads without synchronising
// against that boundary is asserting a coincidence — "the fold happened to be
// complete when the daemon read it" — which is a claim about TIMING, not about
// the system.
//
// Establishing completeness FIRST converts it into a claim about the system.
// The journal is APPEND-ONLY — [session.Journal.Append] only ever extends the
// entry log and Fold walks HEAD back to the root — so a fold observed complete
// STAYS complete. Observing completeness once is therefore sufficient: every
// later read, including the daemon's own read inside the load this call
// precedes, is necessarily complete too. The replay assertion that follows then
// proves what the daemon guarantees, not what the scheduler happened to do.
//
// This is NOT a sleep, a retry, or a poll, and must not be "simplified" into
// one. It blocks on an observable signal — [supervisor.Supervisor.WatchRoster],
// which pushes a snapshot on every roster change — and advances on the one
// transition that means the turn is journaled: the session reporting
// [supervisor.StatusNeedsInput]. That status is only ever published by the
// pump AFTER Session.Prompt returns, and Runner.Prompt returns only after its
// awaitJournaled barrier, i.e. after consume has appended the run's entries.
// The fold is then read exactly ONCE and required to be complete: if that
// inference above is ever wrong, this fails loudly here instead of degrading
// into a flake at the replay assertion.
//
// Call it only after the turn being replayed has been observed to finish;
// before dispatch, a session is legitimately idle with nothing journaled yet.
func awaitFoldComplete(t *testing.T, sup *supervisor.Supervisor, sid string, wantBlocks int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	roster, err := sup.WatchRoster(ctx)
	if err != nil {
		t.Fatalf("WatchRoster: %v", err)
	}
	// lastSeen records the most recent status observed for sid so a timeout can
	// report WHY it never advanced — chiefly to tell apart the two failure modes
	// that are indistinguishable without it (issue #138): a SHORT fold (the turn's
	// entries were not all journaled in time) versus a fold that is already
	// COMPLETE while the needs-input STATUS transition never arrived (a lost/late
	// roster publish, pump-goroutine starvation under load, or an SDK turn that
	// never returned Prompt — not a journaling problem at all).
	lastSeen := "none observed"
	for {
		select {
		case snap, ok := <-roster:
			if !ok {
				t.Fatalf("roster watch closed before session %s reported needs-input\n  fold: %s",
					sid, describeFold(t, sup, sid))
			}
			if st, present := statusOf(snap, sid); present {
				lastSeen = st.String()
			} else {
				lastSeen = "absent from roster"
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
			// Classify the timeout so the next occurrence is conclusive on sight
			// rather than ambiguous (issue #138): if the fold is ALREADY whole, the
			// journal is not the problem — the missing signal is the needs-input
			// status transition, which redirects the investigation away from the
			// journaling window entirely.
			got := foldBlocks(t, sup, sid)
			diagnosis := "the fold is SHORT — the turn's entries were not all journaled within the window"
			if got >= wantBlocks {
				diagnosis = "the fold is already COMPLETE, so journaling is NOT the culprit; the needs-input" +
					" status transition is the signal that never arrived (lost/late roster publish, pump-goroutine" +
					" starvation under load, or an SDK turn that never returned Prompt)"
			}
			t.Fatalf("timed out after %s waiting for session %s to report needs-input"+
				"\n  last status seen: %s"+
				"\n  fold: %s (%d/%d content blocks)"+
				"\n  diagnosis: %s",
				defaultWait, sid, lastSeen, describeFold(t, sup, sid), got, wantBlocks, diagnosis)
		}
	}
}

// statusOf returns sid's status in snap and whether sid was present — the raw
// signal behind [needsInput], exposed so [awaitFoldComplete]'s timeout can report
// the last status it observed (issue #138).
func statusOf(snap []supervisor.SessionInfo, sid string) (supervisor.SessionStatus, bool) {
	for _, s := range snap {
		if s.ID == sid {
			return s.Status, true
		}
	}
	return 0, false
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
// issuing session/load.
func newFoldProbe(t *testing.T, sup *supervisor.Supervisor, sid string) *foldProbe {
	t.Helper()
	return &foldProbe{t: t, sup: sup, sid: sid, before: describeFold(t, sup, sid)}
}

// diagnosis captures the post-load fold snapshot and renders both alongside
// gotKinds — the frames that actually arrived — as a failure-message suffix.
// Call it only on the failure path.
func (p *foldProbe) diagnosis(gotKinds []string) string {
	p.t.Helper()
	return fmt.Sprintf(
		"\n  fold BEFORE load: %s\n  fold AFTER  load: %s\n  frames received : %v"+
			"\n  (a short BEFORE means the history was incomplete AT READ TIME — the replay was"+
			"\n   short at the source; a complete BEFORE means the frames went astray in delivery."+
			"\n   See foldProbe's doc.)",
		p.before, describeFold(p.t, p.sup, p.sid), gotKinds)
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
