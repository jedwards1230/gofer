package config

import (
	"errors"
	"io/fs"
	"os"
	"sync"
)

// CachedLoader returns a [Load] for path that reuses its last result while the
// file on disk is unchanged, and re-reads whenever it is not.
//
// WHY THIS EXISTS. Several TUI settings are resolved by calling the config
// loader on EVERY use rather than sampling it once, deliberately: "always
// current, never a stale snapshot", so an edit from the /config panel or from
// another attached client takes effect on the next frame instead of the next
// process start. The cost of that contract is that a plain [Load] —
// os.ReadFile + json.Unmarshal + validate — runs three times per drawn frame,
// five with an active selection, and six per mouse-motion event (gofer#315,
// item 2). Measured through a real frame render, a 19 KB config (100
// permission rules, 20 MCP servers) costs +1,267 allocations and +127 KB per
// frame that way.
//
// WHY NOT A PLAIN MEMO. A cache that simply remembers the first result would
// break the contract outright — that is precisely the stale snapshot the
// per-call reads exist to avoid. A stale config is also the worst failure shape
// available here: it produces a WRONG result rather than a slow one, and it is
// invisible until someone edits config.json and gofer appears to ignore them.
//
// WHAT COUNTS AS "UNCHANGED", AND THE ONE WINDOW THAT REMAINS. The cached entry
// is keyed on three things together, all from one os.Stat:
//
//   - file identity ([os.SameFile] — device + inode on unix, file index on
//     Windows),
//   - modification time,
//   - size.
//
// mtime alone is NOT enough, and this is a real trap rather than a theoretical
// one: many filesystems store mtime at 1-second resolution, so two writes
// inside the same second can leave mtime identical, and a same-length edit
// leaves size identical too. A (mtime, size) key would then serve a stale
// config indefinitely — until something else about the file happened to change.
//
// Identity is what closes that window for gofer's own writer. [Save] is atomic:
// it writes a temp file in the same directory and renames it over path, so
// every save installs a NEW inode. os.SameFile is therefore false after any
// Save regardless of what the clock or the length did, and the next call
// re-reads.
//
// The residual window is a foreign writer that edits the file IN PLACE
// (truncate-and-write, which keeps the inode) AND lands inside one mtime
// granule of the cached read AND leaves the byte count identical. Accepted
// deliberately: closing it means hashing the contents, which means reading the
// file, which is the work being avoided. Its cost is bounded — the next write
// that differs in any of the three keys re-reads — and gofer's own Save can
// never hit it.
//
// A stat error that is not "does not exist" is not cached at all: the next call
// re-stats and re-reads. Absence IS cached, because it is a legitimate steady
// state (an unconfigured gofer runs fine, and [Load] defines a missing file as
// the zero Config rather than an error) and it is what every fresh install is
// in.
//
// The stat is taken BEFORE the read, never after, for the same reason the
// gofer#298 sidecar cache is: a file that changes between the two is then
// recorded under the OLDER stamp, so the next call re-reads. Stat-after could
// tag content derived from old bytes with the new stamp and pin it.
//
// The returned closure is safe for concurrent use — [Load] is serialized on the
// same mutex as the bookkeeping, so no caller can observe a half-updated entry.
// A closure rather than package-level state on purpose: two loaders over two
// paths share nothing, which is what keeps tests writing configs into their own
// temp directories from contaminating each other.
func CachedLoader(path string) func() (Config, error) {
	var (
		mu sync.Mutex
		// prev is the stat of the file as of the cached read. nil means the
		// cached read observed NO file, which is distinct from !cached.
		prev   os.FileInfo
		cached bool
		cfg    Config
		err    error
	)
	return func() (Config, error) {
		mu.Lock()
		defer mu.Unlock()

		fi, statErr := os.Stat(path)
		switch {
		case cached && statErr == nil && prev != nil &&
			os.SameFile(prev, fi) && prev.ModTime().Equal(fi.ModTime()) && prev.Size() == fi.Size():
			return cfg, err
		case cached && prev == nil && errors.Is(statErr, fs.ErrNotExist):
			return cfg, err
		}

		cfg, err = Load(path)
		switch {
		case statErr == nil:
			prev, cached = fi, true
		case errors.Is(statErr, fs.ErrNotExist):
			prev, cached = nil, true
		default:
			// Something else went wrong reading the directory entry (a
			// permission change, a vanished parent). Cache nothing: Load's own
			// answer for this call still stands, but the next call re-stats
			// rather than trusting an entry keyed on a stat that failed.
			prev, cached = nil, false
		}
		return cfg, err
	}
}
