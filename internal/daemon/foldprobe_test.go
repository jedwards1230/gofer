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
