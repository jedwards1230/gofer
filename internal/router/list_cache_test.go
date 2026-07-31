package router

// list_cache_test.go is the router half of gofer#298. The in-process
// supervisor's List got the sidecar cache first, but under M6 the ROUTER is the
// daemon a TUI or `gofer ps` actually talks to — the in-process path is the
// fallback. A fix that lands only in the supervisor leaves the issue reading as
// closed while a real user on a real daemon pays the identical ~1s-tick cost,
// which is worse than leaving it open: nobody re-measures a closed issue.
//
// The supervisor's own tests demonstrably do NOT cover this — the router kept a
// parallel offline-row builder, and every one of those tests stayed green while
// it re-read every journal on every fetch. So this file exercises the router's
// List directly.
//
// The probe is the same one the supervisor tests use, for the same reason: make
// the cache OBSERVABLE by destroying the journal's CONTENT after warming while
// preserving the size+mtime it was keyed on. A row that still comes back correct
// can only have been served from the sidecar. A timing assertion would be a
// ceiling rather than a proof and would pass against a cache that never engages.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// writeRouterJournal writes one real on-disk journal under root through the
// SDK's own FileStore — a meta root entry carrying cwd, then one user message —
// and returns its id and path. Worker-free on purpose: the assertion is about
// the offline-row builder alone, with no spawn timing in the way.
func writeRouterJournal(t *testing.T, root, cwd, prompt string) (id, path string) {
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
	j, err := store.Create(context.Background(), session.Slugify(cwd))
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if _, err := j.Append(session.NewMetaEntry(cwd)); err != nil {
		t.Fatalf("append meta entry: %v", err)
	}
	if _, err := j.Append(session.NewMessageEntry(provider.UserText(prompt))); err != nil {
		t.Fatalf("append message entry: %v", err)
	}
	id, path = j.ID(), j.Path()
	if err := j.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	return id, path
}

// blankJournalPreservingStat overwrites path with NUL bytes of the same length
// and restores its mtime, so the cache's size+mtime key still matches but the
// file has no parseable entry left. gofer itself cannot produce this state — a
// journal is append-only — which is exactly what makes it a usable probe: the
// two implementations are distinguishable only by whether they read the journal.
func blankJournalPreservingStat(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, fi.Size()), 0o600); err != nil {
		t.Fatalf("blank journal: %v", err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatalf("restore journal mtime: %v", err)
	}
}

// newListRouter returns a router over root, closed on cleanup.
func newListRouter(t *testing.T, root string) *Supervisor {
	t.Helper()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// TestRouterListServesOfflineRowsFromTheSidecarCache is the load-bearing
// assertion for the router path: once warm, a fetch must not read the journal.
func TestRouterListServesOfflineRowsFromTheSidecarCache(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	const prompt = "served from the sidecar"
	id, path := writeRouterJournal(t, root, cwd, prompt)

	// Cold fetch warms the cache.
	if _, err := newListRouter(t, root).List(context.Background()); err != nil {
		t.Fatalf("cold List: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), id+".meta.json")); err != nil {
		t.Fatalf("cold List did not write a sidecar for %s: %v", id, err)
	}

	blankJournalPreservingStat(t, path)

	rows, err := newListRouter(t, root).List(context.Background())
	if err != nil {
		t.Fatalf("warm List: %v", err)
	}
	got := findRouterInfo(rows, id)
	if got == nil {
		t.Fatalf("List missing session %s: %+v", id, rows)
	}
	if got.Title != prompt {
		t.Errorf("Title = %q, want %q — the row was re-derived from the journal, not served from the sidecar", got.Title, prompt)
	}
	if got.Cwd != cwd {
		t.Errorf("Cwd = %q, want %q — the row was re-derived from the journal, not served from the sidecar", got.Cwd, cwd)
	}
	if got.Updated.IsZero() {
		t.Error("Updated is zero — the row was re-derived from the (now blank) journal")
	}
}

// TestRouterOfflineRowStatusMatchesTheSupervisor pins a divergence the
// consolidation fixes, and the reason two parallel builders were a bug rather
// than merely duplication.
//
// The router's own builder never set Status, so every offline row carried the
// ZERO value — which is [supervisor.StatusWorking], not idle. On the primary M6
// deployment path that meant a session at rest reported as working and inflated
// the overview's "N working" counter, while the in-process path (whose builder
// sets StatusIdle deliberately, and says so) reported it correctly. The two
// disagreed about the same session on disk.
func TestRouterOfflineRowStatusMatchesTheSupervisor(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id, _ := writeRouterJournal(t, root, cwd, "at rest")

	rows, err := newListRouter(t, root).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findRouterInfo(rows, id)
	if got == nil {
		t.Fatalf("List missing session %s: %+v", id, rows)
	}
	if got.Live {
		t.Errorf("Live = true, want false (no worker)")
	}
	if got.Status != supervisor.StatusIdle {
		t.Errorf("Status = %v, want StatusIdle — an offline row is a session AT REST; "+
			"the zero value (StatusWorking) reports it as working and inflates the roster's working count",
			got.Status)
	}
}
