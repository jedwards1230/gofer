// Package usercmd loads user-authored markdown slash commands — the files a
// user drops in `<store-root>/commands/` (user scope) or `<cwd>/.gofer/commands/`
// (project scope) to turn a saved prompt into a `/command`. It is a leaf
// package (stdlib only, no bubbletea) so the parsing and substitution rules
// below are testable without a terminal; internal/tui adapts a [Command] into
// a dispatcher entry that submits the expanded body as a prompt.
//
// # Discovery
//
// [Load] walks both directories recursively and takes every `.md` file. A
// nested file becomes a namespaced command — `commands/git/review.md` is
// `/git:review` — using `:` as the separator, matching docs/TUI.md's
// `/skill:name` shape. Dot-prefixed files and directories are skipped
// (editor/OS droppings, never commands), as is any file whose path would not
// produce a legal command token; those are reported as a [Warning] rather
// than failing the load, because one bad file must not cost a user every
// other command they wrote.
//
// Project scope wins over user scope for the same name, with one asymmetry:
// a project file may not claim a name the caller reserves
// ([Options.ReservedForProject]). `<root>/commands` holds files the user
// wrote; `<cwd>/.gofer/commands` holds whatever a cloned repository shipped,
// and those are not the same trust level. Everything else about precedence —
// markdown over builtin, extension over markdown — is the dispatcher
// registry's business, not this package's.
//
// # Frontmatter
//
// A file may open with a `---`-delimited block carrying exactly two
// recognized keys, `description` (the one-line summary the autocomplete popup
// shows) and `argument-hint` (the `[arg]` hint beside the name). Unknown keys
// are ignored so a future key is not a breaking change. This is deliberately
// NOT a YAML parser: two keys do not justify a dependency, and a strict
// `key: value` reader keeps the failure mode obvious. Malformed frontmatter
// (no closing `---`, a line that is not `key: value`) degrades to "this file
// has no frontmatter" plus a [Warning] — the body still becomes a command.
//
// # Substitution
//
// See [Expand] for the token table and every edge case it pins down.
//
// # Cost
//
// [Load] is O(files) syscalls and reads every matching file into memory, each
// bounded by [Options.MaxFileBytes]. It is a once-per-open operation, not a
// per-keystroke one: internal/tui calls it at app construction and again on
// the closed→open transition of the command-autocomplete popup (i.e. once per
// `/` typed, not once per rune), so a command file written while the TUI is
// running shows up the next time the popup opens without the dispatcher ever
// holding a permanently stale view. A typical commands directory is a handful
// of small files; if that ever stops being true the fix is a mtime check
// inside Load, not a cached snapshot in the registry.
//
// The walk is unbounded in TIME, though — a network-mounted cwd can make any
// of those syscalls arbitrarily slow — so the reload after construction runs
// off the UI event loop in a tea.Cmd (internal/tui's loadUserCommandsCmd).
package usercmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Scope is where a command was found. It is part of a command's identity
// because it decides precedence (project beats user) and because it is what
// the default summary says when a file carries no description.
type Scope string

const (
	// ScopeUser is `<store-root>/commands/` — commands available everywhere.
	ScopeUser Scope = "user"
	// ScopeProject is `<cwd>/.gofer/commands/` — commands checked into a repo.
	ScopeProject Scope = "project"
)

// mdExt is the only file extension [Load] treats as a command.
const mdExt = ".md"

// NameSeparator joins the path segments of a nested command file into one
// command token: `commands/git/review.md` → `git:review`.
const NameSeparator = ":"

// Command is one markdown command: its resolved name, the frontmatter fields
// that describe it, and the body that becomes a prompt once [Expand] fills in
// the arguments.
type Command struct {
	// Name is the dispatcher token without its leading slash, with nested
	// directories folded in via [NameSeparator] ("git:review").
	Name string

	// Description is the frontmatter `description`, or a scope-derived
	// default when the file carries none — never empty, because the
	// autocomplete popup renders it as the command's one-line summary.
	Description string

	// ArgumentHint is the frontmatter `argument-hint` ("[pr-number]"), or ""
	// when the command takes no arguments.
	ArgumentHint string

	// Body is the file's content after the frontmatter block, with no
	// substitution applied yet — [Expand] is called at dispatch time against
	// the arguments the user actually typed.
	Body string

	// Path is the file the command was read from, for warnings and debugging.
	Path string

	// Scope is which directory it came from.
	Scope Scope
}

// Warning is one file [Load] could not turn into a command. It is an error
// value so a caller can log or surface it, but it is deliberately not
// returned as the load's error: a bad file is skipped, never fatal.
type Warning struct {
	Path string
	Err  error
}

func (w Warning) Error() string { return w.Path + ": " + w.Err.Error() }

func (w Warning) Unwrap() error { return w.Err }

// UserDir is the user-scope commands directory under a resolved store root.
// root is gofer's `--root`/`~/.gofer`, threaded in rather than recomputed so
// a root override moves the commands directory with it.
func UserDir(root string) string { return filepath.Join(root, "commands") }

// ProjectDir is the project-scope commands directory under a working
// directory.
func ProjectDir(cwd string) string { return filepath.Join(cwd, ".gofer", "commands") }

// Options tunes a [Load]. The zero value is valid: nothing reserved, no size
// cap.
type Options struct {
	// ReservedForProject reports whether name is one the PROJECT scope may not
	// claim. It exists for one trust boundary: `<root>/commands` holds files
	// the USER wrote, so letting them replace a builtin command is a feature;
	// `<cwd>/.gofer/commands` holds whatever a cloned repository happened to
	// ship, and a checked-in `model.md` silently turning /model into "send
	// this text to the agent" is not. A project file whose name is reserved is
	// skipped with a [Warning] naming it — user scope is never checked, so a
	// user's own override of the same builtin still applies.
	//
	// The predicate rather than a name list keeps the builtin set — which is
	// internal/tui's business, aliases and all — out of this package. nil
	// reserves nothing.
	ReservedForProject func(name string) bool

	// MaxFileBytes caps how large one command file may be. A file over the cap
	// is skipped with a [Warning] rather than truncated: half a prompt is not
	// a prompt, and the body is submitted to a model verbatim. Zero or
	// negative means no cap.
	MaxFileBytes int
}

// reservedForProject reports whether name is reserved, tolerating a nil
// predicate.
func (o Options) reservedForProject(name string) bool {
	return o.ReservedForProject != nil && o.ReservedForProject(name)
}

// Load discovers every markdown command under [UserDir](root) and
// [ProjectDir](cwd), with project scope overriding user scope on a name
// collision. Commands come back sorted by Name; warnings carry the files that
// were skipped and why.
//
// An empty root or cwd skips that scope. A missing directory is the normal
// case (most users have neither) and is not a warning. Both arguments
// resolving to the same directory loads it once, as project scope.
//
// Load performs file IO — a stat and a recursive walk per scope, plus a read
// per `.md` file. Callers on a UI event loop must run it off that loop (see
// internal/tui's userCommandsMsg).
func Load(root, cwd string, opts Options) ([]Command, []Warning) {
	var (
		userDir = ""
		projDir = ""
	)
	if root != "" {
		userDir = UserDir(root)
	}
	if cwd != "" {
		projDir = ProjectDir(cwd)
	}
	if userDir != "" && projDir != "" && filepath.Clean(userDir) == filepath.Clean(projDir) {
		// `--root <cwd>/.gofer` makes the two scopes the same directory. Load
		// it once, as the scope that would have won anyway.
		userDir = ""
	}

	byName := map[string]Command{}
	var warns []Warning
	for _, s := range []struct {
		dir   string
		scope Scope
	}{
		{userDir, ScopeUser},
		{projDir, ScopeProject}, // second: project overwrites user
	} {
		if s.dir == "" {
			continue
		}
		cmds, w := loadDir(s.dir, s.scope, opts)
		warns = append(warns, w...)
		for _, c := range cmds {
			byName[c.Name] = c
		}
	}

	out := make([]Command, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, warns
}

// loadDir walks one scope's directory. A missing directory yields nothing at
// all (not a warning); every other failure — an unreadable subtree, an
// unreadable file, an illegal name, malformed frontmatter — yields a warning
// and skips only that entry.
func loadDir(dir string, scope Scope, opts Options) ([]Command, []Warning) {
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []Warning{{Path: dir, Err: err}}
	}

	var (
		cmds  []Command
		warns []Warning
	)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			warns = append(warns, Warning{Path: path, Err: err})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		base := d.Name()
		if path != dir && strings.HasPrefix(base, ".") {
			// Dot-prefixed entries are editor/OS droppings (.DS_Store, .git),
			// never commands. Skipping them silently is the point — warning
			// about them would be noise on every load.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(base), mdExt) {
			return nil
		}
		cmd, warn := loadFile(dir, path, scope, opts)
		if warn != nil {
			warns = append(warns, *warn)
		}
		if cmd != nil {
			cmds = append(cmds, *cmd)
		}
		return nil
	})
	if err != nil {
		warns = append(warns, Warning{Path: dir, Err: err})
	}
	return cmds, warns
}

// loadFile turns one `.md` file into a command. It can return a command AND a
// warning together: malformed frontmatter degrades to "no frontmatter" (the
// body still becomes a usable command) while still reporting what was wrong.
func loadFile(dir, path string, scope Scope, opts Options) (*Command, *Warning) {
	name, err := commandName(dir, path)
	if err != nil {
		return nil, &Warning{Path: path, Err: err}
	}
	if scope == ScopeProject && opts.reservedForProject(name) {
		return nil, &Warning{Path: path, Err: fmt.Errorf(
			"/%s is a builtin command; a project file may not replace one (move it to the user commands directory to override it deliberately)", name)}
	}
	data, err := readCapped(path, opts.MaxFileBytes)
	if err != nil {
		return nil, &Warning{Path: path, Err: err}
	}
	meta, body, err := parseFrontmatter(string(data))
	cmd := Command{
		Name:         name,
		Description:  meta.description,
		ArgumentHint: meta.argumentHint,
		Body:         body,
		Path:         path,
		Scope:        scope,
	}
	if cmd.Description == "" {
		cmd.Description = string(scope) + " markdown command"
	}
	if err != nil {
		return &cmd, &Warning{Path: path, Err: err}
	}
	return &cmd, nil
}

// readCapped reads path, refusing anything larger than maxBytes (<= 0 = no
// cap). The cap is enforced on the open handle with an [io.LimitReader]
// rather than by stat-then-read: a file that grows between the two calls
// would beat the stat, and the read is the thing that actually allocates. It
// also costs one syscall fewer.
//
// It reads one byte PAST the cap so an at-cap file is accepted and an
// over-cap one is detected without a second syscall to size it. The refusal
// is a plain "over the cap" — the exact size isn't known here by
// construction, and isn't what the reader needs to act.
func readCapped(path string, maxBytes int) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the path comes from walking the user's own commands dir
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
		return nil, fmt.Errorf("over the %d-byte command-file cap (tui.max_command_file_bytes)", maxBytes)
	}
	return data, nil
}

// commandName maps a file path under dir to its dispatcher token: the
// dir-relative path minus the `.md` extension, with separators folded to
// [NameSeparator]. It rejects anything that would not survive the dispatcher's
// whitespace-split parse (parseSlash in internal/tui) or would collide with
// the namespace separator, so an illegal filename is a skipped file with a
// reason rather than a command nobody can ever type.
func commandName(dir, path string) (string, error) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	segs := strings.Split(rel, string(filepath.Separator))
	for _, seg := range segs {
		switch {
		case seg == "":
			return "", errors.New("empty path segment")
		case seg == "." || seg == "..":
			return "", fmt.Errorf("illegal path segment %q", seg)
		case strings.ContainsAny(seg, NameSeparator+"/"):
			return "", fmt.Errorf("segment %q contains %q or %q, which the command namespace reserves", seg, NameSeparator, "/")
		case strings.IndexFunc(seg, unicode.IsSpace) >= 0:
			return "", fmt.Errorf("segment %q contains whitespace, so the command could never be typed", seg)
		}
	}
	return strings.Join(segs, NameSeparator), nil
}
