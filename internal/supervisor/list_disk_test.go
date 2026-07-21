package supervisor_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// writeDiskJournal builds a real on-disk journal under root for cwd via a
// throwaway [session.FileStore] — a session_meta entry (see
// [session.NewMetaEntry]) first, then one [session.NewMessageEntry] per msg —
// and closes both the journal and the store before returning, so the journal
// is left purely on disk, exactly as [supervisor.Supervisor.List] finds a
// session that predates the current process.
func writeDiskJournal(t *testing.T, root, cwd string, msgs ...provider.Message) (id, slug, path string) {
	t.Helper()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	}()

	slug = session.Slugify(cwd)
	j, err := store.Create(context.Background(), slug)
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if _, err := j.Append(session.NewMetaEntry(cwd)); err != nil {
		t.Fatalf("append meta entry: %v", err)
	}
	for _, m := range msgs {
		if _, err := j.Append(session.NewMessageEntry(m)); err != nil {
			t.Fatalf("append message entry: %v", err)
		}
	}
	id = j.ID()
	path = j.Path()
	if err := j.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	return id, slug, path
}

// findInfo returns the SessionInfo for id in infos, or nil.
func findInfo(infos []supervisor.SessionInfo, id string) *supervisor.SessionInfo {
	for i := range infos {
		if infos[i].ID == id {
			return &infos[i]
		}
	}
	return nil
}

// TestSupervisor_ListDiskOnlySessionEnrichedFromJournal is THE repro for the
// "session list resets after a daemon restart" bug: a session's journal
// (written by a real [session.FileStore]/[session.NewMetaEntry], simulating
// what runner.New leaves on disk) is enumerated by a Supervisor that never
// resumed it — the exact shape List sees after a daemon restart, before any
// client has re-resumed the session. Before the diskSessionInfo enrichment,
// this disk-only entry carried a zero-value Cwd/Title/Updated, which is what
// silently excluded every offline session from a cwd-filtered session/list.
func TestSupervisor_ListDiskOnlySessionEnrichedFromJournal(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	id, slug, path := writeDiskJournal(t, root, cwd, provider.UserText("investigate the flaky build"))

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing disk-only session %s: %+v", id, infos)
		return
	}
	if got.Live {
		t.Errorf("Live = true, want false (never resumed)")
	}
	if got.Project != slug {
		t.Errorf("Project = %q, want %q", got.Project, slug)
	}
	if got.JournalPath != path {
		t.Errorf("JournalPath = %q, want %q", got.JournalPath, path)
	}
	if got.Cwd != cwd {
		t.Errorf("Cwd = %q, want %q (read from the journal's session_meta entry)", got.Cwd, cwd)
	}
	if got.Title != "investigate the flaky build" {
		t.Errorf("Title = %q, want the first user message's snippet", got.Title)
	}
	if got.Updated.IsZero() {
		t.Error("Updated is zero, want the last entry's time")
	}
	if got.Created.IsZero() {
		t.Error("Created is zero, want the first entry's time")
	}
}

// TestSupervisor_ListLegacyJournalNoMetaDegradesGracefully covers a journal
// written before the SDK persisted cwd: no session_meta root entry, just a
// user message. List must not crash or error — Cwd stays "" (the documented
// fallback lives in the daemon layer), while Title is still recovered from
// the message.
func TestSupervisor_ListLegacyJournalNoMetaDegradesGracefully(t *testing.T) {
	root := t.TempDir()

	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	const slug = "legacy-proj"
	j, err := store.Create(context.Background(), slug)
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if _, err := j.Append(session.NewMessageEntry(provider.UserText("hi"))); err != nil {
		t.Fatalf("append message entry: %v", err)
	}
	id := j.ID()
	if err := j.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing legacy session %s: %+v", id, infos)
		return
	}
	if got.Cwd != "" {
		t.Errorf("Cwd = %q, want empty (no session_meta entry to read)", got.Cwd)
	}
	if got.Title != "hi" {
		t.Errorf("Title = %q, want hi (still recovered from the message)", got.Title)
	}
}

// TestSupervisor_ListCorruptJournalDegradesGracefully covers a journal whose
// interior line is genuinely corrupt (not just a torn final write) —
// [session.ReadEntries] returns [session.ErrCorruptJournal] for it. List must
// still return the bare {ID, Project, JournalPath, Live:false} snapshot for
// that one entry rather than failing the whole call.
func TestSupervisor_ListCorruptJournalDegradesGracefully(t *testing.T) {
	root := t.TempDir()
	const slug = "corrupt-proj"
	dir := filepath.Join(root, "sessions", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const id = "corrupt-id"
	path := filepath.Join(dir, id+".jsonl")
	// Two lines: the first is not valid JSON (an interior line, since a
	// second line follows), which readJournal treats as real corruption
	// rather than a torn final write.
	content := "not valid json\n{\"id\":\"x\",\"type\":\"message\"}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing corrupt-journal session %s: %+v", id, infos)
	}
	if got.Live {
		t.Errorf("Live = true, want false")
	}
	if got.Cwd != "" || got.Title != "" || !got.Updated.IsZero() {
		t.Errorf("corrupt journal enrichment = %+v, want the bare zero-value snapshot", got)
	}
}
