package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"
)

// TestSessionIDPinsSessionNotEntries is the guard test for the session-id
// pinning the worker performs (design Option A): the router pre-generates a
// session's uuid to key the worker's socket/endpoint/lock, and the worker hands
// it to the SDK as [runner.Options.SessionID]. It asserts the two invariants the
// pinning depends on:
//
//	(a) the session adopts the pinned id verbatim, and
//	(b) NO journal ENTRY id collides with it — entry ids keep coming from the
//	    store's own generator.
//
// It exists to fail LOUDLY if a future SDK bump makes entry ids derive from
// SessionID, which would silently desync the worker's socket/endpoint/lock
// keying from journal identity.
//
// # Why the pinned values are deterministic and not UUID-shaped
//
// The ids below are values the store's own generator (UUIDv7) could never emit
// by chance, and the test asserts the two resulting session ids DIFFER FROM EACH
// OTHER. That combination is what actually defeats the "pinning silently became
// a no-op" class: a no-op pin would leave both sessions with generator-drawn
// UUIDv7s, which are neither equal to these literals nor equal to one another.
// Random UUID inputs could not distinguish those cases as sharply — and two
// independent runs that never compare their results add nothing over one.
//
// Both values are valid single path components, so the SDK's id validation
// accepts them and they are safe as journal filenames.
func TestSessionIDPinsSessionNotEntries(t *testing.T) {
	const (
		alpha = "pinned-alpha"
		beta  = "pinned-beta"
	)

	gotAlpha := assertPinned(t, alpha)
	gotBeta := assertPinned(t, beta)

	// The decisive check: two distinct pins must yield two distinct session ids.
	// If pinning no-opped, both would be generator-drawn and this would still
	// differ — but neither would have matched its literal in assertPinned, so the
	// pair of assertions together admits only genuine pinning.
	if gotAlpha == gotBeta {
		t.Fatalf("distinct pins %q and %q both produced session id %q; SessionID is not being honored", alpha, beta, gotAlpha)
	}
}

// assertPinned drives one runner.New through the SessionID seam with pinned,
// asserts invariants (a) and (b) above, and returns the adopted session id.
func assertPinned(t *testing.T, pinned string) string {
	t.Helper()
	root := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := runner.New(ctx, runner.Options{
		Root:      root,
		Cwd:       t.TempDir(), // a non-empty cwd → a valid project slug
		Model:     "faux",
		Provider:  faux.New(faux.Default()),
		SessionID: pinned,
	})
	if err != nil {
		t.Fatalf("runner.New(SessionID=%q): %v", pinned, err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// (a) The session adopted the pinned id verbatim.
	if sess.ID() != pinned {
		t.Fatalf("session id = %q, want the pinned id %q", sess.ID(), pinned)
	}

	// Run a real turn against the deterministic faux provider so the journal
	// accrues message/tool entries on top of the meta entry runner.New writes —
	// every one an independent entry-id draw.
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

	// (b) NO entry id collides with the session id, and entry ids are unique
	// among themselves. Stricter than "at least one differs": that weaker form
	// would pass even if most entries had collapsed onto the session id.
	seen := make(map[string]int, len(entries))
	for i, e := range entries {
		switch e.ID {
		case "":
			t.Errorf("entry %d has an empty id: %+v", i, e)
		case pinned:
			t.Errorf("entry %d id collides with the pinned session id %q; entry ids must stay distinct", i, pinned)
		}
		if prev, dup := seen[e.ID]; dup && e.ID != "" {
			t.Errorf("entry %d duplicates the id of entry %d (%q)", i, prev, e.ID)
		}
		seen[e.ID] = i
	}

	return sess.ID()
}
