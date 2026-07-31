package config_test

// cachedloader_test.go covers the two halves of [config.CachedLoader]'s
// contract, which pull in opposite directions: it must NOT re-read an unchanged
// file (that is the whole point), and it MUST re-read a changed one (that is
// the "always current, never a stale snapshot" property the per-call config
// reads exist to provide — a memo that broke it would be a regression dressed
// as an optimization).
//
// This is the riskiest change in its PR, because it is the only one that can
// produce a WRONG result rather than a slow one, and a stale-config bug is
// invisible until someone edits config.json and gofer appears to ignore them.
// So the coverage is written around the TRANSITIONS rather than the steady
// state: absent → created, present → modified, present → deleted, and the
// same-mtime same-size atomic rewrite that a naive (mtime, size) key cannot
// see.
//
// The read count is observed through the filesystem rather than a counter seam:
// the loader is handed a real path, and the test edits the real file.

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
)

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// touchDistinct rewrites path and forces a modification time that differs from
// whatever it had, so the test is not at the mercy of the filesystem's
// timestamp granularity. Without it, two writes inside one granule can leave an
// identical stamp and the "sees the edit" assertion would flake — and, worse,
// would flake in the direction of PASSING when the loader is broken, since a
// loader with no cache at all also passes it.
func touchDistinct(t *testing.T, path, body string, when time.Time) {
	t.Helper()
	writeConfig(t, path, body)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestCachedLoaderServesUnchangedFileFromCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"session":{"model":"claude-sonnet-5"}}`)

	load := config.CachedLoader(path)
	first, err := load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Replace the file's CONTENT while restoring its stat identity — same size,
	// same mtime. A cache that is really keyed on the file's stamp cannot see
	// this, and must keep serving the first result. A loader that re-read every
	// time would return the new model and fail here.
	//
	// This is the only way to prove a cache HIT from outside: any observation
	// that distinguishes "cached" from "re-read" has to make the two answers
	// differ, and the file is the only input.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	writeConfig(t, path, `{"session":{"model":"claude-sonnet-6"}}`)
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if newFi, err := os.Stat(path); err != nil {
		t.Fatalf("re-stat: %v", err)
	} else if newFi.Size() != fi.Size() {
		t.Fatalf("test setup: replacement is %d bytes, want the original %d — the stamp differs for the wrong reason", newFi.Size(), fi.Size())
	}

	second, err := load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second.Session.Model != first.Session.Model {
		t.Errorf("loader re-read a file whose mtime and size were unchanged: model %q -> %q; the memo is not caching at all", first.Session.Model, second.Session.Model)
	}
}

func TestCachedLoaderSeesAnEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	base := time.Now().Add(-time.Hour)
	touchDistinct(t, path, `{"session":{"model":"claude-sonnet-5"}}`, base)

	load := config.CachedLoader(path)
	if got, err := load(); err != nil {
		t.Fatalf("first load: %v", err)
	} else if got.Session.Model != "claude-sonnet-5" {
		t.Fatalf("first load model = %q", got.Session.Model)
	}

	touchDistinct(t, path, `{"session":{"model":"claude-opus-5"}}`, base.Add(time.Minute))

	got, err := load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got.Session.Model != "claude-opus-5" {
		t.Errorf("model = %q after the file changed, want claude-opus-5 — the memo is serving a STALE snapshot, which is exactly the contract the per-call config reads exist to keep", got.Session.Model)
	}
}

// TestCachedLoaderSeesASameMtimeResize covers the SIZE half of the stamp,
// which mtime alone does not: a rewrite that lands inside the filesystem's
// timestamp granule leaves the mtime identical, and a stamp keyed only on
// mtime would serve the old config indefinitely. Here the mtime is pinned
// identical explicitly so the only thing that can invalidate the entry is the
// length.
//
// This test exists because it was missing: a mutation that dropped size from
// the stamp left every other test in this file green.
func TestCachedLoaderSeesASameMtimeResize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	when := time.Now().Add(-time.Hour)
	touchDistinct(t, path, `{"session":{"model":"claude-opus-5"}}`, when)

	load := config.CachedLoader(path)
	if got, err := load(); err != nil {
		t.Fatalf("first load: %v", err)
	} else if got.Session.Model != "claude-opus-5" {
		t.Fatalf("first load model = %q", got.Session.Model)
	}

	// Longer body, same mtime.
	touchDistinct(t, path, `{"session":{"model":"claude-opus-5","effort":"high"}}`, when)

	got, err := load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got.Session.Effort != "high" {
		t.Errorf("effort = %q after a same-mtime rewrite that changed the file's LENGTH, want \"high\" — the cache stamp is not keyed on size, so an edit inside one timestamp granule is invisible", got.Session.Effort)
	}
}

// TestCachedLoaderSeesAFileAppearing covers the state a fresh install is in:
// no config file at all. Load defines that as the zero Config and no error, so
// the loader caches "absent" — and must still notice the file being created,
// which is what `gofer` writing its first config.json does.
func TestCachedLoaderSeesAFileAppearing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	load := config.CachedLoader(path)
	got, err := load()
	if err != nil {
		t.Fatalf("load with no file: %v", err)
	}
	if got.Session.Model != "" {
		t.Fatalf("load with no file = %+v, want the zero Config", got)
	}

	writeConfig(t, path, `{"session":{"model":"claude-opus-5"}}`)

	got, err = load()
	if err != nil {
		t.Fatalf("load after create: %v", err)
	}
	if got.Session.Model != "claude-opus-5" {
		t.Errorf("model = %q after the config file was created, want claude-opus-5 — the cached absent-file result is never invalidated", got.Session.Model)
	}
}

// TestCachedLoaderSeesAnAtomicSaveInsideOneMtimeGranule is the requirement that
// mtime+size alone cannot meet, and the reason file IDENTITY is part of the key.
//
// Many filesystems store mtime at 1-second resolution. Two config.Save calls
// inside the same second, writing bodies of the same length, leave mtime AND
// size unchanged — so a (mtime, size) key would serve the first config
// indefinitely, until something unrelated about the file happened to change.
// The test forces exactly that: identical mtime (explicitly), identical size
// (asserted, not assumed), different content.
//
// config.Save is atomic — temp file plus rename — so the second save installs a
// new inode, and os.SameFile is what notices. This is the mechanism that makes
// gofer's OWN writes always visible to the cache regardless of clock
// granularity.
func TestCachedLoaderSeesAnAtomicSaveInsideOneMtimeGranule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	when := time.Now().Add(-time.Hour)

	save := func(model string) {
		t.Helper()
		if err := config.Save(path, config.Config{Session: config.Session{Model: model}}); err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	// Two ids of equal length, so the serialized files are byte-for-byte the
	// same size.
	save("claude-sonnet-5")
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	load := config.CachedLoader(path)
	if got, err := load(); err != nil {
		t.Fatalf("first load: %v", err)
	} else if got.Session.Model != "claude-sonnet-5" {
		t.Fatalf("first load model = %q", got.Session.Model)
	}

	save("claude-sonnet-6")
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("re-stat: %v", err)
	}
	// Assert the trap is actually set. If the two saves differed in size or
	// mtime, this test would pass against a loader keyed on nothing but those,
	// and would prove nothing about identity.
	if second.Size() != first.Size() {
		t.Fatalf("test setup: sizes differ (%d vs %d) — the mtime+size key would catch this on its own", second.Size(), first.Size())
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Fatalf("test setup: mtimes differ (%s vs %s) — the mtime key would catch this on its own", second.ModTime(), first.ModTime())
	}
	if os.SameFile(first, second) {
		t.Fatal("test setup: config.Save did NOT replace the inode, so there is no identity change to detect — is Save still atomic?")
	}

	got, err := load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got.Session.Model != "claude-sonnet-6" {
		t.Errorf("model = %q after an atomic Save with an identical mtime and size, want claude-sonnet-6 — the cache key does not include file identity, so a config written twice inside one filesystem timestamp granule is served STALE indefinitely", got.Session.Model)
	}
}

// TestCachedLoaderSeesASameSizeInPlaceEdit is the third key dimension, and the
// one mtime alone carries.
//
// A foreign writer that edits in place — an editor's truncate-and-write, a
// `sed -i` that keeps the byte count — leaves BOTH the inode and the size
// unchanged, so identity and size are blind to it and only the modification
// time moves. It is an easy edit to make by hand ("high" → "none", one model id
// for another of equal length), which is why the key carries all three.
//
// Like the identity test above, this exists because a mutation dropping mtime
// from the key left every other test in this file green.
func TestCachedLoaderSeesASameSizeInPlaceEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	base := time.Now().Add(-time.Hour)
	// os.WriteFile (not config.Save) on purpose: it opens the existing path
	// O_TRUNC, so the inode survives, which is what makes this the in-place
	// case rather than the atomic-replace one.
	touchDistinct(t, path, `{"session":{"model":"claude-sonnet-5"}}`, base)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	load := config.CachedLoader(path)
	if got, err := load(); err != nil {
		t.Fatalf("first load: %v", err)
	} else if got.Session.Model != "claude-sonnet-5" {
		t.Fatalf("first load model = %q", got.Session.Model)
	}

	touchDistinct(t, path, `{"session":{"model":"claude-sonnet-6"}}`, base.Add(time.Minute))
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("re-stat: %v", err)
	}
	// Assert the trap is set: only the mtime may differ.
	if !os.SameFile(before, after) {
		t.Fatalf("test setup: the rewrite replaced the inode — the identity key would catch this on its own")
	}
	if after.Size() != before.Size() {
		t.Fatalf("test setup: sizes differ (%d vs %d) — the size key would catch this on its own", after.Size(), before.Size())
	}

	got, err := load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got.Session.Model != "claude-sonnet-6" {
		t.Errorf("model = %q after a same-inode same-size in-place edit, want claude-sonnet-6 — the cache key does not include the modification time, so an edit that changes only the CONTENT is invisible", got.Session.Model)
	}
}

// TestCachedLoaderSeesADeletion covers the present → absent transition. Load
// defines a missing file as the zero Config rather than an error, so a deleted
// config must fall back to defaults rather than keep serving the settings of a
// file that no longer exists.
func TestCachedLoaderSeesADeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"session":{"model":"claude-opus-5"}}`)

	load := config.CachedLoader(path)
	if got, err := load(); err != nil {
		t.Fatalf("first load: %v", err)
	} else if got.Session.Model != "claude-opus-5" {
		t.Fatalf("first load model = %q", got.Session.Model)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}

	got, err := load()
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if got.Session.Model != "" {
		t.Errorf("model = %q after config.json was DELETED, want the zero Config — the cache is serving settings from a file that no longer exists", got.Session.Model)
	}
}

// TestCachedLoaderConcurrent is the race check the memo needs because its
// callers are not one goroutine: the TUI reads it from the bubbletea loop while
// Cmd goroutines read it too, and sessionGuard reads config six times per
// session create.
//
// Two assertions the race detector does not make on its own: no caller ever
// observes a value that was never written (a torn or half-updated cache entry),
// and the writer's changes ARE observed under contention — a locking bug could
// pin the entry only when readers and the writer overlap, which the
// single-goroutine tests above cannot see.
//
// WHY THE READERS RUN TO A CONDITION AND NOT A FIXED COUNT. The first version
// gave each reader a fixed quota and asserted afterwards that both values had
// been seen. That is a race, and it lost: a cached read is a single stat, so
// 1,600 of them complete in ~3.8ms, while one config.Save — marshal, temp file,
// chmod, write, rename — takes ~6.5ms. The readers reliably finished their
// whole quota before the writer landed its FIRST alternate save, so every read
// returned the initial value and the assertion failed on correct code.
//
// Reading until the change is observed (or a deadline expires) turns that race
// into a bounded wait. The second value normally appears within ~13ms, so the
// deadline is ~750x the expected time — it exists to fail a genuinely pinned
// cache, not to pace the test.
func TestCachedLoaderConcurrent(t *testing.T) {
	const (
		modelA   = "claude-sonnet-5"
		modelB   = "claude-opus-5"
		readers  = 8
		deadline = 10 * time.Second
	)

	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{Session: config.Session{Model: modelA}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	load := config.CachedLoader(path)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
		stop = make(chan struct{})
	)
	bothSeen := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen[modelA] > 0 && seen[modelB] > 0
	}
	expired := time.After(deadline)

	// The writer replaces the file through the real atomic Save, which is the
	// write path gofer actually uses.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			model := modelA
			if i%2 == 1 {
				model = modelB
			}
			if err := config.Save(path, config.Config{Session: config.Session{Model: model}}); err != nil {
				t.Errorf("save: %v", err)
				return
			}
		}
	}()

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cfg, err := load()
				if err != nil {
					t.Errorf("load: %v", err)
					return
				}
				mu.Lock()
				seen[cfg.Session.Model]++
				mu.Unlock()
			}
		}()
	}

	// Stop everything as soon as the change has been observed under contention,
	// or give up at the deadline and let the assertion below report it.
	timedOut := false
	for done := false; !done; {
		select {
		case <-expired:
			timedOut, done = true, true
		default:
			if bothSeen() {
				done = true
			} else {
				time.Sleep(time.Millisecond)
			}
		}
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for model, n := range seen {
		total += n
		if model != modelA && model != modelB {
			t.Errorf("observed model %q (%d times) — neither of the two values ever written; a concurrent reader saw a torn or half-updated cache entry", model, n)
		}
	}
	if timedOut {
		t.Errorf("after %s and %d concurrent reads the writer's change was never observed (saw %v); the cache is pinning a value under contention rather than re-reading", deadline, total, seen)
	}
}

// TestCachedLoaderPropagatesAndClearsErrors pins that the memo does not turn a
// malformed config into a silent zero Config, and does not pin the error either:
// Load treats a parse failure as an error on purpose (a typo in a permission
// rule must fail loudly rather than silently widening the policy), so the memo
// has to carry that error AND stop carrying it once the file is fixed.
func TestCachedLoaderPropagatesAndClearsErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	base := time.Now().Add(-time.Hour)
	touchDistinct(t, path, `{"session":{`, base)

	load := config.CachedLoader(path)
	if _, err := load(); err == nil {
		t.Fatal("load of a malformed config returned no error")
	}
	if _, err := load(); err == nil {
		t.Fatal("the cached load of a malformed config returned no error — the memo dropped it")
	}

	touchDistinct(t, path, `{"session":{"model":"claude-opus-5"}}`, base.Add(time.Minute))

	got, err := load()
	if err != nil {
		t.Fatalf("load after the config was fixed: %v", err)
	}
	if got.Session.Model != "claude-opus-5" {
		t.Errorf("model = %q after the malformed config was fixed, want claude-opus-5", got.Session.Model)
	}
}
