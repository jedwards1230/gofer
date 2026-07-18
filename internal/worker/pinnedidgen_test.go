package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/worker"
)

// TestPinnedIDGenPinsSessionNotEntries is the guard test for the session-id
// pinning BRIDGE (see [worker.PinnedIDGen]). It creates a session through the
// pinned generator against the vendored SDK and asserts the two invariants the
// bridge depends on:
//
//	(a) the session id equals the pinned uuid (the store's first id draw), and
//	(b) at least one journal ENTRY id is distinct from it (later draws fall
//	    through to fresh UUIDv7s, so entry ids never collide).
//
// It exists to fail LOUDLY if a future SDK bump reorders id generation so the
// first draw is no longer the session id — which would silently desync the
// worker's socket/endpoint/lock keying from its actual session id.
func TestPinnedIDGenPinsSessionNotEntries(t *testing.T) {
	root := t.TempDir()
	pinned := uuid.Must(uuid.NewV7()).String()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := runner.New(ctx, runner.Options{
		Root:     root,
		Cwd:      t.TempDir(), // a non-empty cwd → a valid project slug
		Model:    "faux",
		Provider: faux.New(faux.Default()),
		IDGen:    worker.PinnedIDGen(pinned),
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// (a) The session id is the pinned uuid — the store's FIRST id draw.
	if sess.ID() != pinned {
		t.Fatalf("session id = %q, want the pinned uuid %q", sess.ID(), pinned)
	}

	// Run a real turn against the deterministic faux provider so the journal
	// accrues message/tool entries on top of the meta entry runner.New writes —
	// every one an id draw AFTER the first.
	if err := sess.Prompt(ctx, "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	entries, err := session.ReadEntries(sess.JournalPath())
	if err != nil {
		t.Fatalf("ReadEntries(%s): %v", sess.JournalPath(), err)
	}
	if len(entries) == 0 {
		t.Fatal("journal has no entries; expected at least the meta entry")
	}

	// (b) At least one entry id is a distinct, non-empty UUIDv7 — proof the
	// pinned id was NOT reused for entries.
	var sawDistinct bool
	for _, e := range entries {
		if e.ID == "" {
			t.Errorf("entry has empty id: %+v", e)
		}
		if e.ID != pinned {
			sawDistinct = true
		}
	}
	if !sawDistinct {
		t.Fatalf("every journal entry id equals the pinned session id %q; entry ids must stay distinct", pinned)
	}
}
