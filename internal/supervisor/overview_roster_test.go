package supervisor_test

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// hashJournals returns a map of every `<id>.jsonl` path under root/sessions to
// the SHA-256 of its bytes — the oracle for "the rebuild is READ-ONLY over the
// journals". The `.meta.json` sidecars are deliberately excluded: archiving
// writes a sidecar, and that is exactly the NON-journal write the persistence
// model relies on.
func hashJournals(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	sessionsDir := filepath.Join(root, "sessions")
	err := filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out[path] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk journals: %v", err)
	}
	return out
}

// TestOverviewRosterRebuildsFromJournals is the data-loss fix's core assertion:
// a Supervisor that never ran any of these sessions rebuilds the overview roster
// from their on-disk journals (the exact shape a daemon restart produces), and
// an ARCHIVED session stays off it while its journal is untouched. This is what
// makes "restart the daemon, the sessions that were showing come back" true.
func TestOverviewRosterRebuildsFromJournals(t *testing.T) {
	root := t.TempDir()

	keptA, _, _ := writeDiskJournal(t, root, t.TempDir(), provider.UserText("ship the release notes"))
	keptB, _, _ := writeDiskJournal(t, root, t.TempDir(), provider.UserText("triage the flaky test"))
	archived, _, archivedPath := writeDiskJournal(t, root, t.TempDir(), provider.UserText("old spike, done"))

	// Mark the third session archived on disk — a sidecar write, exactly what
	// Supervisor.Archive records, with no live supervisor involved.
	if found, err := supervisor.SetArchivedOnDisk(root, archived, true, time.Now()); err != nil || !found {
		t.Fatalf("SetArchivedOnDisk(%s) = found %v, err %v; want found true, no error", archived, found, err)
	}

	before := hashJournals(t, root)

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	// OverviewRoster: the two non-archived sessions are restored (offline,
	// Live=false, enriched from their journals); the archived one is absent.
	overview, err := sup.OverviewRoster(context.Background())
	if err != nil {
		t.Fatalf("OverviewRoster: %v", err)
	}
	if got := findInfo(overview, keptA); got == nil {
		t.Errorf("OverviewRoster missing non-archived session %s: %+v", keptA, overview)
	} else {
		if got.Live {
			t.Errorf("%s Live = true, want false (never resumed)", keptA)
		}
		if got.Title != "ship the release notes" {
			t.Errorf("%s Title = %q, want the journal's first-message snippet (enrichment must survive)", keptA, got.Title)
		}
		if got.Updated.IsZero() {
			t.Errorf("%s Updated is zero, want the last entry's time (enrichment must survive)", keptA)
		}
	}
	if findInfo(overview, keptB) == nil {
		t.Errorf("OverviewRoster missing non-archived session %s: %+v", keptB, overview)
	}
	if got := findInfo(overview, archived); got != nil {
		t.Errorf("OverviewRoster includes archived session %s = %+v, want it hidden", archived, got)
	}

	// List still carries the archived session (so `gofer ps --all` and the
	// resume picker can see it), now flagged Archived.
	list, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findInfo(list, archived)
	if got == nil {
		t.Fatalf("List dropped archived session %s: %+v", archived, list)
	}
	if !got.Archived {
		t.Errorf("List entry for %s has Archived = false, want true", archived)
	}

	// The archived session's journal is byte-identical — archiving wrote a
	// sidecar, never the JSONL.
	if _, err := os.Stat(archivedPath); err != nil {
		t.Fatalf("archived journal vanished: %v", err)
	}
	if diff := journalDiff(before, hashJournals(t, root)); diff != "" {
		t.Errorf("journals changed by the rebuild/archive: %s", diff)
	}
}

// TestOverviewRosterLeavesJournalsUntouched is the data-safety guarantee: the
// whole persistence path — rebuilding the roster, listing, and archiving an
// offline session — never deletes or truncates a single journal. It snapshots
// every journal's bytes, drives each read/archive path, and asserts the bytes
// are identical afterward.
func TestOverviewRosterLeavesJournalsUntouched(t *testing.T) {
	root := t.TempDir()

	a, _, _ := writeDiskJournal(t, root, t.TempDir(), provider.UserText("one"))
	b, _, _ := writeDiskJournal(t, root, t.TempDir(), provider.UserText("two"))

	before := hashJournals(t, root)
	if len(before) != 2 {
		t.Fatalf("expected 2 journals on disk, snapshotted %d", len(before))
	}

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	if _, err := sup.OverviewRoster(context.Background()); err != nil {
		t.Fatalf("OverviewRoster: %v", err)
	}
	if _, err := sup.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	// Archive an OFFLINE session (the post-restart path): writes its sidecar,
	// never its journal.
	if err := sup.Archive(context.Background(), a); err != nil {
		t.Fatalf("Archive(%s): %v", a, err)
	}
	_ = b

	after := hashJournals(t, root)
	if len(after) != len(before) {
		t.Errorf("journal count changed: before %d, after %d — a journal was created or deleted", len(before), len(after))
	}
	if diff := journalDiff(before, after); diff != "" {
		t.Errorf("a journal's bytes changed: %s", diff)
	}
}

// TestArchivePersistsAcrossReload is the archive half of the persistence model:
// archiving a session keeps it off the overview roster even after the daemon
// (supervisor) is dropped and rebuilt — proving the marker is durable, not
// just in-memory roster removal.
func TestArchivePersistsAcrossReload(t *testing.T) {
	root := t.TempDir()
	id, _, path := writeDiskJournal(t, root, t.TempDir(), provider.UserText("archive me"))

	// First supervisor: archive the offline session, then drop it.
	sup1, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	if err := sup1.Archive(context.Background(), id); err != nil {
		t.Fatalf("Archive(%s): %v", id, err)
	}
	_ = sup1.Close()

	// Second supervisor over the same root — the restart. The archived session
	// must stay off the overview, still be on List, and keep its journal.
	sup2, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New (reload): %v", err)
	}
	t.Cleanup(func() { _ = sup2.Close() })

	overview, err := sup2.OverviewRoster(context.Background())
	if err != nil {
		t.Fatalf("OverviewRoster: %v", err)
	}
	if got := findInfo(overview, id); got != nil {
		t.Errorf("archived session %s reappeared on the overview after reload: %+v", id, got)
	}
	list, err := sup2.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := findInfo(list, id); got == nil || !got.Archived {
		t.Errorf("List after reload = %+v for %s, want it present with Archived=true", got, id)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("archived session's journal was removed: %v", err)
	}
}

// TestResumeClearsArchiveMarker proves a reloaded session is resumable AND that
// resuming an archived one brings it back to the overview for good: Resume
// clears the durable archive marker, so the session is no longer hidden on the
// next restart. Without the clear, a resumed session would vanish again the next
// time the roster rebuilt from disk.
func TestResumeClearsArchiveMarker(t *testing.T) {
	h := newHarness(t)
	cwd := t.TempDir()

	// Plant an ON-DISK journal and archive it — an offline, archived session,
	// the shape a restart leaves behind for one the operator had archived.
	id, _, _ := writeDiskJournal(t, h.root, cwd, provider.UserText("bring me back"))
	if found, err := supervisor.SetArchivedOnDisk(h.root, id, true, time.Now()); err != nil || !found {
		t.Fatalf("SetArchivedOnDisk(%s) = found %v, err %v", id, found, err)
	}
	if !supervisor.DiskArchived(h.root, id) {
		t.Fatalf("precondition: %s should read back archived", id)
	}

	// Resume it into the same cwd (so its directory — hence its sidecar — is the
	// one the marker lives beside).
	if _, err := h.sup.Resume(context.Background(), id, supervisor.ResumeOptions{Cwd: cwd, Model: "m"}); err != nil {
		t.Fatalf("Resume(%s): %v", id, err)
	}

	// The marker is cleared: the session stays visible after a future restart.
	if supervisor.DiskArchived(h.root, id) {
		t.Errorf("archive marker for %s survived a resume, want it cleared", id)
	}

	// And it is live on the overview now.
	overview, err := h.sup.OverviewRoster(context.Background())
	if err != nil {
		t.Fatalf("OverviewRoster: %v", err)
	}
	got := findInfo(overview, id)
	if got == nil {
		t.Fatalf("resumed session %s absent from the overview: %+v", id, overview)
	}
	if !got.Live {
		t.Errorf("resumed session %s Live = false, want true", id)
	}
}

// journalDiff reports the first journal whose bytes changed (or appeared/
// vanished) between two snapshots, or "" when they match.
func journalDiff(before, after map[string][32]byte) string {
	for path, h := range before {
		ah, ok := after[path]
		if !ok {
			return path + " was DELETED"
		}
		if ah != h {
			return path + " CHANGED"
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			return path + " was CREATED"
		}
	}
	return ""
}
