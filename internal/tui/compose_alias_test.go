package tui

import (
	"reflect"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestWithHelpersDoNotAliasBaseModel pins the ONE value-semantics surface that
// survives gofer#308: the render-local composition helpers.
//
// [Model.Ingest] used to deep-copy the transcript on every event so that a prior
// Model stayed observable. That made replay quadratic (1.6s / ~9GB at 5,000
// turns) and it could not be made cheap while keeping the guarantee — a
// MessageDelta writes THROUGH to an existing element (m.items[idx].text += …),
// which no spare-capacity or ownership trick avoids. So Ingest took a pointer
// receiver and now owns its transcript outright.
//
// What that leaves is this: [App.View] composes render-local blocks onto the
// live model — WithThinking, WithBackgroundAgents, WithShellRuns, WithCompacting
// — and those STILL return copies. Each allocates a fresh items array before
// appending, and that allocation is now the only thing standing between a
// transient indicator and the durable transcript. Ingest's growth leaves spare
// capacity in base.items, so a helper that appended into the shared array
// instead would silently (a) clobber a sibling composition's tail and (b) be
// clobbered in turn by the next ingested event. Both are asserted below.
//
// The mutation evidence for this test, and for the quadratic copy it replaced,
// is recorded in docs/TESTING.md.
func TestWithHelpersDoNotAliasBaseModel(t *testing.T) {
	const s = "sess-compose"

	base := ingested(theme.Test(),
		event.NewMessageFinished(s, event.MessageUser, "hello"),
		event.NewMessageStarted(s, event.MessageText),
		event.NewMessageFinished(s, event.MessageText, "hi"),
		event.NewToolCallStarted(s, "call-1", "bash", nil),
		event.NewTurnStarted(s),
	)
	if cap(base.items) <= len(base.items) {
		t.Fatalf("precondition: base.items has no spare capacity (len=%d cap=%d) — "+
			"a shared-array append would be undetectable here", len(base.items), cap(base.items))
	}
	baseItems := append([]item(nil), base.items...)

	// Two render-local compositions off the SAME base. Both append exactly one
	// trailing block, so a shared backing array would put them at the same index.
	thinking := base.WithThinking()
	agents := base.WithBackgroundAgents([]SessionInfo{{ID: "child-1", Agent: "reviewer"}})
	if got := len(thinking.items) - len(baseItems); got != 1 {
		t.Fatalf("precondition: WithThinking appended %d items, want 1", got)
	}
	if got := len(agents.items) - len(baseItems); got != 1 {
		t.Fatalf("precondition: WithBackgroundAgents appended %d items, want 1", got)
	}

	// 1. Neither composition reached the base's durable transcript.
	if !reflect.DeepEqual(base.items, baseItems) {
		t.Errorf("base.items mutated by a render-local composition:\n got %+v\nwant %+v", base.items, baseItems)
	}

	// 2. The two compositions did not clobber each other's tail.
	if k := thinking.items[len(thinking.items)-1].kind; k != itemThinking {
		t.Errorf("WithThinking's tail is kind %v, want itemThinking — WithBackgroundAgents overwrote it through a shared array", k)
	}
	if k := agents.items[len(agents.items)-1].kind; k != itemBackgroundAgents {
		t.Errorf("WithBackgroundAgents' tail is kind %v, want itemBackgroundAgents — WithThinking overwrote it through a shared array", k)
	}

	// 3. The next ingested event appends into the base's spare capacity. That
	// must not reach either composition — this is the failure a user would see
	// as a "thinking" indicator turning into somebody else's transcript row.
	base.Ingest(event.NewMessageFinished(s, event.MessageUser, "next turn"))
	if k := thinking.items[len(thinking.items)-1].kind; k != itemThinking {
		t.Errorf("WithThinking's tail is kind %v after an Ingest on the base, want itemThinking", k)
	}
	if k := agents.items[len(agents.items)-1].kind; k != itemBackgroundAgents {
		t.Errorf("WithBackgroundAgents' tail is kind %v after an Ingest on the base, want itemBackgroundAgents", k)
	}
}
