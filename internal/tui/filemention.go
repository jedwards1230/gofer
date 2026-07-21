package tui

// filemention.go implements the `@` input prefix: typing `@` at a token
// boundary in either text-entry surface opens the same popup the slash
// commands use (command_menu.go), sourced from the paths under the session's
// working directory instead of the command registry.
//
// SCOPE DECISION — `@path` passes the PATH through, it does not inline the
// file. A submitted `@internal/tui/app.go` reaches the model as that literal
// text and nothing more; the agent reads it with its own file tools if it
// wants the contents. The alternative (splicing the file's bytes into the
// prompt) is a silent, unbounded, unauditable context charge on a keystroke —
// exactly what CLAUDE.md's context-cost discipline forbids — and gofer's
// agent already has read tools, so inlining would buy nothing but tokens.
// `@` is therefore a completion affordance: it makes the path correct and
// fast to type, and costs the length of the path.
//
// The enumeration runs OFF the Update loop, like every other blocking read in
// this package ([App.discoverModelsCmd]), and is bounded on both sides —
// entries and depth (config's tui.file_mention_max_entries /
// tui.file_mention_max_depth) — so a mention typed inside a home directory or
// a vendored monorepo can't hang the UI or eat memory.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
)

// fileCandidates is [App]'s cached `@`-completion source. It is refreshed once
// per mention rather than per keystroke (loaded flips false again the moment
// the `@` token goes away, see [App.syncFileCandidates]), which keeps the list
// fresh enough to include a file the agent just wrote while never paying for
// the walk on ordinary typing.
type fileCandidates struct {
	paths   []string
	loading bool
	loaded  bool
}

// filesLoadedMsg carries a finished candidate enumeration back onto the
// Update loop. It has no error field on purpose: every failure mode already
// degrades to a shorter (possibly empty) list inside [listFileCandidates],
// and "the popup shows nothing" is the honest rendering of "there was nothing
// to offer" — the user can always type the path themselves.
type filesLoadedMsg struct{ paths []string }

// syncFileCandidates decides whether buf's state should kick off an
// enumeration. It is called from [App.syncMenu] on every keystroke, so the
// cheap path — no `@` token active — must stay cheap: it only resets the
// cache flag so the NEXT mention re-enumerates.
func (a App) syncFileCandidates(buf inputBuffer) (App, tea.Cmd) {
	sigil, _, _, ok := activeToken(buf.String(), buf.Cursor())
	if !ok || sigil != '@' {
		if !a.files.loading {
			a.files.loaded = false
		}
		return a, nil
	}
	if a.files.loading || a.files.loaded {
		return a, nil
	}
	a.files.loading = true
	return a, a.fileCandidatesCmd()
}

// applyFilesLoaded folds a finished enumeration into the cache and reopens the
// popup against it — the load was dispatched because an `@` token was active,
// and the user has typed nothing since unless another key press already
// resynced.
func (a App) applyFilesLoaded(msg filesLoadedMsg) (App, tea.Cmd) {
	a.files = fileCandidates{paths: msg.paths, loaded: true}
	return a.syncMenu()
}

// fileCandidatesCmd enumerates the mention candidates off the Update loop,
// bounded by the live config's entry/depth limits and by the same child-
// process timeout the `!` escape uses (tui.shell_timeout_ms — the git listing
// below is a child process too).
func (a App) fileCandidatesCmd() tea.Cmd {
	cwd := a.commandEnv.Cwd
	maxEntries, maxDepth := a.fileMentionLimits()
	timeout := a.shellTimeout()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return filesLoadedMsg{paths: listFileCandidates(ctx, cwd, maxEntries, maxDepth)}
	}
}

// fileMentionLimits resolves tui.file_mention_max_entries and
// tui.file_mention_max_depth off the live config on every call, never cached —
// the same contract [App.autoscrollEnabled] follows. A nil Config closure or a
// read error falls through to the built-in defaults.
func (a App) fileMentionLimits() (maxEntries, maxDepth int) {
	if a.commandEnv.Config == nil {
		return config.DefaultFileMentionMaxEntries, config.DefaultFileMentionMaxDepth
	}
	cfg, err := a.commandEnv.Config()
	if err != nil {
		return config.DefaultFileMentionMaxEntries, config.DefaultFileMentionMaxDepth
	}
	return cfg.TUI.FileMentionEntryLimit(), cfg.TUI.FileMentionDepthLimit()
}

// listFileCandidates returns the mention candidates under cwd, cwd-relative
// and slash-separated, sorted.
//
// It asks git first ([gitListFiles]): inside a repository, `git ls-files` is
// both the fastest enumeration available and the only one that honors
// .gitignore without gofer taking on a matcher dependency — and a completion
// list that offers node_modules/ or a build/ tree is worse than no list at
// all. Outside a repository (or when git isn't installed, or the command
// fails) it falls back to a bounded [filepath.WalkDir] that can only skip
// .git by name. Both paths honor maxEntries; only the walk has a depth to
// honor. An empty cwd yields nothing rather than enumerating the process's
// working directory, which is not the session's.
func listFileCandidates(ctx context.Context, cwd string, maxEntries, maxDepth int) []string {
	if cwd == "" {
		return nil
	}
	if paths, ok := gitListFiles(ctx, cwd, maxEntries); ok {
		return paths
	}
	return walkFiles(cwd, maxEntries, maxDepth)
}

// gitListFiles lists cwd's tracked and untracked-but-not-ignored files via
// git. ok is false when cwd isn't in a work tree, git isn't available, or the
// command failed — every one of which means "fall back to the walk", not
// "there are no files".
func gitListFiles(ctx context.Context, cwd string, maxEntries int) ([]string, bool) {
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	fields := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	paths := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		paths = append(paths, filepath.ToSlash(f))
		if len(paths) >= maxEntries {
			break
		}
	}
	if len(paths) == 0 {
		// An empty repo (or a cwd with nothing but ignored files) is a real
		// answer, not a failure — but so is `git ls-files` succeeding in a
		// directory git considers empty of anything interesting. Reporting ok
		// here keeps the walk from re-scanning a tree git already said is
		// entirely ignored.
		return nil, true
	}
	sort.Strings(paths)
	return paths, true
}

// walkFiles is the non-git fallback: a bounded [filepath.WalkDir] under cwd,
// skipping .git outright (it is never a mention target and dwarfs most trees)
// and stopping at maxDepth directory levels or maxEntries collected files,
// whichever comes first. It cannot honor .gitignore — that is exactly why the
// git path above is tried first.
func walkFiles(cwd string, maxEntries, maxDepth int) []string {
	var paths []string
	_ = filepath.WalkDir(cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree costs its own entries, never the walk.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(cwd, path)
		if relErr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if strings.Count(rel, "/")+1 >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, rel)
		if len(paths) >= maxEntries {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(paths)
	return paths
}

// matchFilePaths filters paths to those matching partial (case-insensitive),
// best matches first, capped at limit rows.
//
// Ranking is three-tier because a path is not a command name: a user typing
// `@app` almost always means a file NAMED app-something, not the first
// alphabetical path that happens to contain "app" in a directory component.
// So a whole-path prefix wins, then a base-name prefix, then anything
// containing the text at all; ties break lexicographically, which
// [sort.SliceStable] preserves from the already-sorted input.
func matchFilePaths(paths []string, partial string, limit int) []string {
	q := strings.ToLower(partial)
	type scored struct {
		path string
		rank int
	}
	matches := make([]scored, 0, len(paths))
	for _, p := range paths {
		lower := strings.ToLower(p)
		switch {
		case q == "" || strings.HasPrefix(lower, q):
			matches = append(matches, scored{p, 0})
		case strings.HasPrefix(strings.ToLower(filepath.Base(p)), q):
			matches = append(matches, scored{p, 1})
		case strings.Contains(lower, q):
			matches = append(matches, scored{p, 2})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].rank < matches[j].rank })
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.path
	}
	return out
}
