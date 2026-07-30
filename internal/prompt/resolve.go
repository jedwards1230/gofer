package prompt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// resolvedEntry is where one [config.Prompt.Files] entry points, decided
// before any read: its dedup identity (see [Compose]) and how to fetch its
// content.
type resolvedEntry struct {
	// key is the dedup identity: "builtin:<name>" for a builtin source, else
	// the resolved filesystem path.
	key string
	// builtin, when non-empty, selects the embedded asset by name instead of
	// a file.
	builtin string
	// path is the filesystem path to read when builtin == "".
	path string
}

// resolveEntry implements the resolution rules documented on
// [config.Prompt]: "builtin:<name>" loads an embedded asset (the only
// non-path form); an absolute path is used verbatim; a "~/…" path expands
// against the user's home directory; any other relative path is resolved
// against cwd first, then storeRoot — first hit wins.
//
// Existence, not readability, decides precedence for the last case: a stat
// failure for a reason OTHER than "missing" (e.g. permissions) still counts
// as a candidate found there, so the resulting read error names the right
// file. When NEITHER candidate exists, the cwd candidate is reported (root's
// if cwd is empty) so a caller's warning names a sensible path rather than
// silently picking neither.
func resolveEntry(entry, cwd, storeRoot string) resolvedEntry {
	if name, ok := strings.CutPrefix(entry, "builtin:"); ok {
		return resolvedEntry{key: entry, builtin: name}
	}
	if filepath.IsAbs(entry) {
		return resolvedEntry{key: entry, path: entry}
	}
	if home, ok := expandHome(entry); ok {
		return resolvedEntry{key: home, path: home}
	}

	if cwd != "" {
		candidate := filepath.Join(cwd, entry)
		if pathExists(candidate) {
			return resolvedEntry{key: candidate, path: candidate}
		}
	}
	if storeRoot != "" {
		candidate := filepath.Join(storeRoot, entry)
		if pathExists(candidate) {
			return resolvedEntry{key: candidate, path: candidate}
		}
	}
	missing := filepath.Join(storeRoot, entry)
	if cwd != "" {
		missing = filepath.Join(cwd, entry)
	}
	return resolvedEntry{key: missing, path: missing}
}

// expandHome expands a "~" or "~/…" entry against the user's home directory.
// ok is false for anything not spelled that way, or when the home directory
// can't be resolved (an entry that looks like "~/x" but can't expand is
// treated as an ordinary relative path instead of hard-failing).
func expandHome(entry string) (string, bool) {
	if entry != "~" && !strings.HasPrefix(entry, "~/") {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	if entry == "~" {
		return home, true
	}
	return filepath.Join(home, strings.TrimPrefix(entry, "~/")), true
}

// pathExists reports whether path names something on disk, without regard to
// what it is or whether it is readable.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// read fetches r's content: the embedded asset for a builtin source, or the
// file at r.path capped at maxBytes (<= 0 = no cap).
func (r resolvedEntry) read(maxBytes int) (string, error) {
	if r.builtin != "" {
		text, ok := builtinAsset(r.builtin)
		if !ok {
			return "", fmt.Errorf("no builtin prompt asset %q", r.builtin)
		}
		return text, nil
	}
	data, err := readCapped(r.path, maxBytes)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// readCapped reads path, refusing anything larger than maxBytes (<= 0 = no
// cap) — the same discipline internal/usercmd's readCapped uses for command
// files, for the same reason: the cap is enforced on the open handle with an
// [io.LimitReader] rather than by stat-then-read, so a file that grows
// between the two calls can't beat it, and reading one byte past the cap
// detects an over-cap file without a second syscall to size it.
func readCapped(path string, maxBytes int) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from gofer's own config.Prompt.Files, a trusted local source
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: a Close error says nothing about the bytes already read
	if maxBytes <= 0 {
		return io.ReadAll(f)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("over the %d-byte prompt-file cap (prompt.max_file_bytes)", maxBytes)
	}
	return data, nil
}
