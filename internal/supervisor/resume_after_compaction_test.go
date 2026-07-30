package supervisor_test

// resume_after_compaction_test.go covers the resume-after-compaction case
// the M7 plan calls out explicitly: a session that has been compacted must
// resume correctly — the compacted (short) context, not the original
// pre-compaction transcript, is what a reloaded session folds and continues
// from.
//
// It drives a REAL *runner.Runner (not the fakeSession used elsewhere in
// this package) over a scripted faux provider, exactly like
// TestSupervisor_Integration_RealRunner: no network, fully deterministic.
// Compaction is triggered EXPLICITLY (Supervisor.Compact) rather than
// through the pump's automatic trigger — resume-safety is a property of the
// SDK's journal/Fold contract plus Supervisor.Resume's wiring, and is
// agnostic to WHICH path fired Compact; the trigger decision itself (under/
// at/over threshold, auto-disabled, unknown context window) is covered
// exhaustively by compact_test.go's fakeSession-based table and integration
// tests, which do not need a real provider round trip.

import (
	"context"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSupervisor_Integration_ResumeAfterCompaction drives one turn, compacts
// it away, closes the session (simulating a daemon restart — Kill keeps the
// on-disk journal per architecture invariant #4), resumes it fresh from
// disk, and asserts the resumed session's folded history shows ONLY the
// summary — then drives a further turn to prove the resumed session is
// genuinely usable, not just readable.
func TestSupervisor_Integration_ResumeAfterCompaction(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	// Three scripted turns: the original conversation turn, the compaction
	// summarizer's own call (Compact's default Summarizer makes one plain
	// completion call over the folded messages), and a turn run AFTER resume
	// to prove the reloaded session still works.
	prov := faux.New(faux.Script{Turns: []faux.Turn{
		{
			Text:       []string{"Hello there."},
			Usage:      provider.Usage{InputTokens: 9, OutputTokens: 7},
			StopReason: provider.StopEndTurn,
		},
		{
			Text:       []string{"Condensed summary of the conversation above."},
			Usage:      provider.Usage{InputTokens: 40, OutputTokens: 6},
			StopReason: provider.StopEndTurn,
		},
		{
			Text:       []string{"Continuing after resume."},
			Usage:      provider.Usage{InputTokens: 12, OutputTokens: 5},
			StopReason: provider.StopEndTurn,
		},
	}})

	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	newSup := func() *supervisor.Supervisor {
		sup, err := supervisor.New(supervisor.Config{
			Root:  root,
			Store: store,
			NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
				opts.Store = store
				opts.Provider = prov
				return runner.New(ctx, opts)
			},
			ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
				opts.Store = store
				opts.Provider = prov
				return runner.Resume(ctx, id, opts)
			},
		})
		if err != nil {
			t.Fatalf("supervisor.New: %v", err)
		}
		return sup
	}

	ctx := context.Background()
	sup := newSup()
	defer func() { _ = sup.Close() }()

	entry, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: cwd, Model: "faux-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sub, err := sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForTurnFinished(t, sub)
	waitForStatus(t, sup, entry.ID, supervisor.StatusNeedsInput)

	preCompact, err := sup.History(ctx, entry.ID)
	if err != nil {
		t.Fatalf("History before compact: %v", err)
	}
	if len(preCompact) == 0 {
		t.Fatal("History before compact is empty — nothing to compact")
	}

	// Explicit compaction: consumes the script's second turn as the
	// summarizer's own call.
	if err := sup.Compact(ctx, entry.ID, "condense"); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertEventKind(t, sub, event.KindSessionCompacted)

	postCompact, err := sup.History(ctx, entry.ID)
	if err != nil {
		t.Fatalf("History after compact: %v", err)
	}
	if len(postCompact) != 1 {
		t.Fatalf("History after compact = %d messages, want exactly 1 (the summary)", len(postCompact))
	}
	if !messagesContain(postCompact, "Condensed summary") {
		t.Fatalf("History after compact does not contain the summary: %+v", postCompact)
	}

	// Simulate a daemon restart: Kill releases the live runner (keeping the
	// journal — invariant #4) so a fresh Resume genuinely reads it back off
	// disk rather than returning the idempotent already-live snapshot.
	if err := sup.Kill(ctx, entry.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	if _, err := sup.Resume(ctx, entry.ID, supervisor.ResumeOptions{Cwd: cwd}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	postResume, err := sup.History(ctx, entry.ID)
	if err != nil {
		t.Fatalf("History after resume: %v", err)
	}
	if len(postResume) != len(postCompact) {
		t.Fatalf("History after resume = %d messages, want %d (the compaction boundary must survive resume) — got %+v",
			len(postResume), len(postCompact), postResume)
	}
	if !messagesContain(postResume, "Condensed summary") {
		t.Fatalf("History after resume does not contain the summary: %+v", postResume)
	}
	if messagesContain(postResume, "Hello there") {
		t.Fatalf("History after resume still contains the pre-compaction turn — the compaction boundary did not survive resume: %+v", postResume)
	}

	// The resumed session must still be genuinely usable: a further prompt
	// runs the script's third turn cleanly.
	sub2, err := sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe after resume: %v", err)
	}
	if err := sup.Send(ctx, entry.ID, "continue"); err != nil {
		t.Fatalf("Send after resume: %v", err)
	}
	waitForTurnFinished(t, sub2)
	waitForStatus(t, sup, entry.ID, supervisor.StatusNeedsInput)
}

// messagesContain reports whether any text block across msgs contains substr.
func messagesContain(msgs []provider.Message, substr string) bool {
	for _, m := range msgs {
		for _, c := range m.Content {
			if strings.Contains(c.Text, substr) {
				return true
			}
		}
	}
	return false
}
