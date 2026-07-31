package supervisor_test

// list_cache_test.go covers the sidecar cache of an offline row's
// journal-derived metadata (gofer#298): OverviewRoster/List used to parse every
// non-live session's ENTIRE journal on every refresh, so a ~1s TUI tick paid
// O(sessions x journal length) continuously.
//
// The property under test is not "it is fast" — a timing assertion would be a
// ceiling, not a proof, and would pass against a cache that never engages.
// Instead each test makes the cache OBSERVABLE by destroying the journal after
// warming: a row that still comes back correct with its journal deleted or
// truncated can only have been served from the sidecar. The complement — a
// journal that CHANGED must be re-derived — is proven the same way, by
// appending to a warm session and checking the row moves.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// newListSupervisor returns a Supervisor over root, closed on cleanup.
func newListSupervisor(t *testing.T, root string) *supervisor.Supervisor {
	t.Helper()
	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// readDerived returns the `derived` object of id's sidecar under dir, and
// whether the sidecar has one at all. It reads the raw JSON rather than going
// through an exported reader on purpose: the sidecar is a visible on-disk
// artifact, so the assertion is on its bytes.
func readDerived(t *testing.T, dir, id string) (map[string]any, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, id+".meta.json"))
	if err != nil {
		return nil, false
	}
	var side struct {
		Derived map[string]any `json:"derived"`
	}
	if err := json.Unmarshal(raw, &side); err != nil {
		t.Fatalf("unmarshal sidecar %s: %v", raw, err)
	}
	return side.Derived, side.Derived != nil
}

// TestListColdReadPopulatesTheSidecarCache is the warm-up half: a session that
// has only a journal (no sidecar at all — the pre-upgrade state, and the state
// every root session is in) must, after ONE List, have its derived metadata
// persisted beside the journal keyed on that journal's size and mtime. Without
// this, a read-only cache would never engage and the fix would be inert.
func TestListColdReadPopulatesTheSidecarCache(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id, _, path := writeDiskJournal(t, root, cwd, provider.UserText("warm the cache"))
	dir := filepath.Dir(path)

	if _, ok := readDerived(t, dir, id); ok {
		t.Fatalf("sidecar already carries derived metadata before any List")
	}

	sup := newListSupervisor(t, root)
	if _, err := sup.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}

	derived, ok := readDerived(t, dir, id)
	if !ok {
		t.Fatalf("List did not persist derived metadata for %s", id)
	}
	if got := derived["title"]; got != "warm the cache" {
		t.Errorf("cached title = %v, want %q", got, "warm the cache")
	}
	if got := derived["cwd"]; got != cwd {
		t.Errorf("cached cwd = %v, want %q", got, cwd)
	}

	// The staleness key must be the journal's ACTUAL stat, or the cache would
	// either never hit or hit when it must not.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	if got, want := derived["journalSize"], float64(fi.Size()); got != want {
		t.Errorf("cached journalSize = %v, want %v", got, want)
	}
	if got, want := derived["journalModNano"], float64(fi.ModTime().UnixNano()); got != want {
		t.Errorf("cached journalModNano = %v, want %v", got, want)
	}
}

// TestListWarmReadServesFromSidecarWithoutTheJournal is the load-bearing
// assertion: once warm, a listing must not read the journal. It is made
// observable by DELETING the journal's contents after warming — the file is
// truncated to zero bytes but its size/mtime key is restored, so any
// implementation that still parses the JSONL produces an empty row, while one
// serving the sidecar returns the metadata intact.
//
// (Truncate-then-restore-stat is a situation gofer itself cannot produce — a
// journal is append-only. That is exactly what makes it a usable probe: it can
// only be distinguished by whether the journal was read.)
func TestListWarmReadServesFromSidecarWithoutTheJournal(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id, _, path := writeDiskJournal(t, root, cwd, provider.UserText("served from the sidecar"))

	sup := newListSupervisor(t, root)
	if _, err := sup.List(context.Background()); err != nil {
		t.Fatalf("warm List: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	// Blank the journal's CONTENT while preserving the size+mtime the cache was
	// keyed on: same bytes-on-disk length, no parseable entries.
	if err := os.WriteFile(path, make([]byte, fi.Size()), 0o600); err != nil {
		t.Fatalf("blank journal: %v", err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatalf("restore journal mtime: %v", err)
	}

	infos, err := newListSupervisor(t, root).List(context.Background())
	if err != nil {
		t.Fatalf("warm List: %v", err)
	}
	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing session %s: %+v", id, infos)
	}
	if got.Title != "served from the sidecar" {
		t.Errorf("Title = %q, want %q — the row was re-derived from the journal, not served from the sidecar",
			got.Title, "served from the sidecar")
	}
	if got.Cwd != cwd {
		t.Errorf("Cwd = %q, want %q — the row was re-derived from the journal, not served from the sidecar", got.Cwd, cwd)
	}
	if got.Updated.IsZero() {
		t.Error("Updated is zero — the row was re-derived from the (now empty) journal")
	}
}

// TestListAppendedJournalInvalidatesTheCache is the correctness complement: a
// cache that never invalidates would pin a stale title and a frozen last-
// activity time onto every session that keeps being used. Appending a turn must
// move Updated and (for a journal whose first user message is new) the title.
func TestListAppendedJournalInvalidatesTheCache(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id, _, path := writeDiskJournal(t, root, cwd, provider.UserText("first prompt"))

	sup := newListSupervisor(t, root)
	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("warm List: %v", err)
	}
	warm := findInfo(infos, id)
	if warm == nil {
		t.Fatalf("List missing session %s", id)
	}
	before := warm.Updated

	// Append a later turn through the real store, exactly as a resumed session
	// would. The appended entry carries a strictly later timestamp.
	appendDiskTurn(t, root, id, provider.UserText("a later turn"), before.Add(time.Hour))

	infos, err = newListSupervisor(t, root).List(context.Background())
	if err != nil {
		t.Fatalf("post-append List: %v", err)
	}
	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing session %s after append", id)
	}
	if !got.Updated.After(before) {
		t.Errorf("Updated = %v, want later than the pre-append %v — the stale cache was served", got.Updated, before)
	}

	// And the cache must have been rewritten against the NEW stat, or every
	// subsequent listing would keep re-deriving.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	derived, ok := readDerived(t, filepath.Dir(path), id)
	if !ok {
		t.Fatalf("sidecar lost its derived metadata after the append")
	}
	if got, want := derived["journalSize"], float64(fi.Size()); got != want {
		t.Errorf("cached journalSize = %v, want %v (the re-derive did not rewrite the key)", got, want)
	}
}

// TestListLegacySidecarWithoutDerivedFallsBackToTheJournal covers the upgrade
// path: a sidecar written by an older gofer carries a subagent link and no
// `derived` object at all. The row must still list correctly (from the journal),
// and the older fields must survive the cache write-back rather than being
// clobbered by it.
func TestListLegacySidecarWithoutDerivedFallsBackToTheJournal(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id, _, path := writeDiskJournal(t, root, cwd, provider.UserText("legacy sidecar session"))
	dir := filepath.Dir(path)

	// Exactly the bytes a pre-#298 gofer wrote: no "derived" key.
	legacy := `{"parentId":"parent-1","agent":"go-developer","depth":1}`
	if err := os.WriteFile(filepath.Join(dir, id+".meta.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy sidecar: %v", err)
	}

	infos, err := newListSupervisor(t, root).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing legacy-sidecar session %s: %+v", id, infos)
	}
	if got.Title != "legacy sidecar session" {
		t.Errorf("Title = %q, want it recovered from the journal", got.Title)
	}
	if got.Cwd != cwd {
		t.Errorf("Cwd = %q, want %q", got.Cwd, cwd)
	}
	if got.ParentID != "parent-1" || got.Agent != "go-developer" || got.Depth != 1 {
		t.Errorf("row = {parent %q, agent %q, depth %d}, want the legacy sidecar's link preserved",
			got.ParentID, got.Agent, got.Depth)
	}

	// The write-back must have ADDED derived without dropping the legacy fields.
	raw, err := os.ReadFile(filepath.Join(dir, id+".meta.json"))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var side struct {
		ParentID string         `json:"parentId"`
		Agent    string         `json:"agent"`
		Depth    int            `json:"depth"`
		Derived  map[string]any `json:"derived"`
	}
	if err := json.Unmarshal(raw, &side); err != nil {
		t.Fatalf("unmarshal sidecar %s: %v", raw, err)
	}
	if side.ParentID != "parent-1" || side.Agent != "go-developer" || side.Depth != 1 {
		t.Errorf("sidecar after cache write-back = %+v, want the legacy link preserved", side)
	}
	if side.Derived == nil {
		t.Error("sidecar gained no derived metadata — a legacy sidecar never warms")
	}
}

// TestListUnreadableJournalCachesNothing pins the documented degrade: a corrupt
// journal still produces the bare {ID, Project, JournalPath} snapshot, and —
// critically — nothing is cached for it, so a journal that is repaired (or was
// only transiently unreadable) recovers on the next listing instead of being
// pinned empty forever by its own cache entry.
func TestListUnreadableJournalCachesNothing(t *testing.T) {
	root := t.TempDir()
	const slug = "corrupt-cache-proj"
	dir := filepath.Join(root, "sessions", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const id = "corrupt-cache-id"
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte("not valid json\n{\"id\":\"x\",\"type\":\"message\"}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	infos, err := newListSupervisor(t, root).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing corrupt-journal session %s: %+v", id, infos)
	}
	if got.Cwd != "" || got.Title != "" || !got.Updated.IsZero() {
		t.Errorf("corrupt journal enrichment = %+v, want the bare zero-value snapshot", got)
	}
	if derived, ok := readDerived(t, dir, id); ok {
		t.Errorf("an unreadable journal cached %v, want no cache entry at all", derived)
	}
}

// TestListDoesNotResurrectAnArchivedSessionUnderConcurrency guards the hazard
// the cache introduces: List is now a WRITER of the same sidecar Archive
// writes. A refresh that read the sidecar before the archive landed and wrote
// it back afterward would silently clear the marker and put the session back on
// the overview. The two are driven concurrently, repeatedly, and the archived
// session must never reappear.
func TestListDoesNotResurrectAnArchivedSessionUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	ids := make([]string, 0, 8)
	paths := make([]string, 0, 8)
	for range 8 {
		id, _, path := writeDiskJournal(t, root, t.TempDir(), provider.UserText("concurrent archive"))
		ids = append(ids, id)
		paths = append(paths, path)
	}

	sup := newListSupervisor(t, root)
	ctx := context.Background()

	// Hammer List while archiving each session, so a refresh is in flight
	// across the archive's read-modify-write.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := sup.OverviewRoster(ctx); err != nil {
				return
			}
		}
	}()
	for i, id := range ids {
		// Touch the journal so the next refresh finds the cache stale and takes
		// the re-derive-and-write-back path — the one that can clobber.
		now := time.Now().Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(paths[i], now, now); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if err := sup.Archive(ctx, id); err != nil {
			t.Fatalf("Archive(%s): %v", id, err)
		}
	}
	close(stop)
	<-done

	overview, err := sup.OverviewRoster(ctx)
	if err != nil {
		t.Fatalf("OverviewRoster: %v", err)
	}
	for _, id := range ids {
		if got := findInfo(overview, id); got != nil {
			t.Errorf("archived session %s reappeared on the overview: %+v — a concurrent cache write-back cleared its marker", id, got)
		}
	}
}

// appendDiskTurn appends one message entry to an existing on-disk journal
// through the real [session.FileStore] (Open, not Create), then closes it —
// the shape a resumed session leaves behind. at is the entry's timestamp,
// pinned through the store's clock seam so the assertion does not depend on
// wall-clock resolution.
func appendDiskTurn(t *testing.T, root, id string, msg provider.Message, at time.Time) {
	t.Helper()
	store, err := session.NewFileStore(
		session.WithRoot(root),
		session.WithStoreClock(func() time.Time { return at }),
	)
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	}()
	j, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", id, err)
	}
	if _, err := j.Append(session.NewMessageEntry(msg)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}
