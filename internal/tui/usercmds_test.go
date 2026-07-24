package tui

// usercmds_test.go covers the REGISTRY half of markdown commands: that a
// loaded file becomes a listable, lookupable command, and that the layered
// precedence (extension > markdown > builtin, project > user) resolves
// deterministically rather than by map iteration order. White-box because
// [Registry]'s layering and [App.registry] are internal — the dispatch and
// autocomplete behavior a user actually sees is exercised through the
// exported surface in usercmds_send_test.go.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/usercmd"
)

// writeUserCmd creates a markdown command file under dir, making parents.
func writeUserCmd(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// newUserCmdApp builds an App whose CommandEnv points at fresh temp store
// root and cwd, after seeding is done by the caller.
func newUserCmdApp(t *testing.T, root, cwd string) App {
	t.Helper()
	env := GoldenCommandEnv()
	env.Root, env.Cwd = root, cwd
	return NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), env)
}

// reloadUserCommands drives one full off-loop reload synchronously: build the
// Cmd the "/" edge dispatches, run it, and fold the resulting message back in
// exactly as [App.Update] does.
func reloadUserCommands(t *testing.T, a App) App {
	t.Helper()
	msg, ok := a.loadUserCommandsCmd()().(userCommandsMsg)
	if !ok {
		t.Fatalf("loadUserCommandsCmd returned %T, want userCommandsMsg", msg)
	}
	return a.applyUserCommands(msg)
}

// listNames returns every non-Hidden command name in registration-independent
// sorted order ([Registry.List] already sorts).
func listNames(r Registry) []string {
	names := make([]string, 0, len(r.List()))
	for _, c := range r.List() {
		names = append(names, c.Name)
	}
	return names
}

// TestUserCommandsAppearInRegistry verifies a markdown file becomes a real
// registry entry — listed for autocomplete, resolvable by [Registry.Lookup],
// carrying its frontmatter into Summary/ArgHint — alongside every builtin.
func TestUserCommandsAppearInRegistry(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "git/review.md",
		"---\ndescription: Review a PR\nargument-hint: [pr]\n---\nreview PR $1\n")

	a := newUserCmdApp(t, root, cwd)

	got, ok := a.registry.Lookup("git:review")
	if !ok {
		t.Fatalf("Lookup(git:review) failed; registry has %v", listNames(a.registry))
	}
	if got.Summary != "Review a PR" || got.ArgHint != "[pr]" {
		t.Errorf("command = %+v; want the frontmatter description/argument-hint", got)
	}
	if got.Source != sourceMarkdown {
		t.Errorf("Source = %v; want sourceMarkdown", got.Source)
	}
	if names := listNames(a.registry); !slices.Contains(names, "git:review") {
		t.Errorf("List() = %v; want it to contain git:review", names)
	}
	// The builtins must still be there — markdown is a layer above them, not
	// a replacement for them.
	builtin, ok := a.registry.Lookup("model")
	if !ok {
		t.Fatal("Lookup(model) failed; the builtin layer was lost")
	}
	if builtin.Source != sourceBuiltin {
		t.Errorf("/model Source = %v; want sourceBuiltin — Source's zero value is what an un-annotated Command literal registers as", builtin.Source)
	}
}

// TestExtensionTierOutranksMarkdown exercises the RESERVED top tier. Plugin
// commands are P1 and not built, but the ordering they will need is part of
// this registry's contract now (docs/TUI.md's "extension > markdown >
// builtin"), and an ordering nothing asserts is an ordering that silently
// rots before the feature that depends on it arrives.
func TestExtensionTierOutranksMarkdown(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "review.md", "---\ndescription: from markdown\n---\nbody\n")

	a := newUserCmdApp(t, root, cwd)
	a.registry.setLayer(sourceExtension, []Command{{
		Name:    "review",
		Summary: "from an extension",
		Run:     func(app App, _ []string) (App, tea.Cmd) { return app, nil },
	}})

	got, ok := a.registry.Lookup("review")
	if !ok {
		t.Fatal("Lookup(review) failed")
	}
	if got.Summary != "from an extension" {
		t.Fatalf("/review = %+v; want the extension layer to outrank markdown", got)
	}
	if n := count(listNames(a.registry), "review"); n != 1 {
		t.Fatalf("List() has %d /review rows; want exactly 1", n)
	}
}

// TestUserCommandShadowsBuiltin is the precedence rule from docs/TUI.md:
// markdown outranks builtin. A `status.md` must WIN /status outright — not
// tie, not lose, and not leave two /status rows in the autocomplete list.
func TestUserCommandShadowsBuiltin(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "status.md",
		"---\ndescription: My own status prompt\n---\nsummarize where we are\n")

	a := newUserCmdApp(t, root, cwd)

	got, ok := a.registry.Lookup("status")
	if !ok {
		t.Fatal("Lookup(status) failed")
	}
	if got.Source != sourceMarkdown || got.Summary != "My own status prompt" {
		t.Fatalf("/status = %+v; want the markdown file to shadow the builtin", got)
	}
	if !slices.Contains(listNames(a.registry), "status") {
		t.Fatal("List() lost /status entirely")
	}
	if n := count(listNames(a.registry), "status"); n != 1 {
		t.Fatalf("List() has %d /status rows; want exactly 1 — a shadowed builtin must disappear, not double up", n)
	}
}

// TestUserCommandShadowsBuiltinAlias covers the subtler half of shadowing: a
// markdown /config must also take the builtin's "cfg" alias, or the builtin
// stays reachable under a name the user thought they had overridden — and
// [Registry.List] (which deduplicates by Name) would then pick between the
// two by map iteration order.
func TestUserCommandShadowsBuiltinAlias(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "config.md",
		"---\ndescription: My own config prompt\n---\nwhat is configured?\n")

	a := newUserCmdApp(t, root, cwd)

	for _, token := range []string{"config", "cfg"} {
		got, ok := a.registry.Lookup(token)
		if !ok {
			t.Fatalf("Lookup(%s) failed", token)
		}
		if got.Source != sourceMarkdown {
			t.Errorf("Lookup(%s).Source = %v; want the markdown command to own the builtin's alias too", token, got.Source)
		}
	}
	// Deterministic across runs: repeated builds must never disagree, which
	// is exactly what map-iteration-order resolution would do.
	for range 20 {
		if got, _ := newUserCmdApp(t, root, cwd).registry.Lookup("config"); got.Source != sourceMarkdown {
			t.Fatal("Lookup(config) resolved to the builtin on a repeat build — precedence is order-dependent")
		}
	}
}

// TestProjectCommandShadowsUserCommand pins the scope half of precedence at
// the registry level (usercmd_test.go covers the loader's own resolution).
func TestProjectCommandShadowsUserCommand(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "review.md", "---\ndescription: user\n---\nuser body\n")
	writeUserCmd(t, usercmd.ProjectDir(cwd), "review.md", "---\ndescription: project\n---\nproject body\n")

	a := newUserCmdApp(t, root, cwd)

	got, ok := a.registry.Lookup("review")
	if !ok {
		t.Fatal("Lookup(review) failed")
	}
	if got.Summary != "project" {
		t.Fatalf("/review summary = %q; want the project file to shadow the user one", got.Summary)
	}
	if n := count(listNames(a.registry), "review"); n != 1 {
		t.Fatalf("List() has %d /review rows; want exactly 1", n)
	}
}

// TestUserCommandReloadDropsDeletedFile is the other half of "not a
// permanently stale registry": a reload must REPLACE the markdown layer, not
// accumulate into it, so a deleted file's command disappears.
func TestUserCommandReloadDropsDeletedFile(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "gone.md", "body")
	writeUserCmd(t, usercmd.UserDir(root), "stays.md", "body")

	a := newUserCmdApp(t, root, cwd)
	if _, ok := a.registry.Lookup("gone"); !ok {
		t.Fatal("Lookup(gone) failed before the delete")
	}

	if err := os.Remove(filepath.Join(usercmd.UserDir(root), "gone.md")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeUserCmd(t, usercmd.UserDir(root), "fresh.md", "body")
	a = reloadUserCommands(t, a)

	if _, ok := a.registry.Lookup("gone"); ok {
		t.Error("Lookup(gone) still resolves after its file was deleted — the reload accumulated instead of replacing")
	}
	for _, name := range []string{"stays", "fresh"} {
		if _, ok := a.registry.Lookup(name); !ok {
			t.Errorf("Lookup(%s) failed after reload", name)
		}
	}
}

// TestProjectCommandCannotShadowBuiltin is the trust boundary: a markdown
// file in `<cwd>/.gofer/commands` is whatever a cloned repository shipped, so
// it may NOT replace a builtin — a checked-in `model.md` silently turning
// /model into "send this text to the agent" is the attack this refuses. The
// refusal is reported, and every unreserved project command still loads.
func TestProjectCommandCannotShadowBuiltin(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.ProjectDir(cwd), "model.md", "---\ndescription: hijacked\n---\nsend this instead\n")
	writeUserCmd(t, usercmd.ProjectDir(cwd), "deploy.md", "---\ndescription: fine\n---\nship it\n")

	a := newUserCmdApp(t, root, cwd)

	got, ok := a.registry.Lookup("model")
	if !ok {
		t.Fatal("Lookup(model) failed; the builtin was lost entirely")
	}
	if got.Source != sourceBuiltin {
		t.Fatalf("/model = %+v; want the BUILTIN — a project file must not replace one", got)
	}
	if deploy, ok := a.registry.Lookup("deploy"); !ok || deploy.Source != sourceMarkdown {
		t.Errorf("Lookup(deploy) = %+v, %v; want the unreserved project command to load normally", deploy, ok)
	}
	if a.status == "" || a.statusSev != sevWarn {
		t.Errorf("status = %q (sev %v); want a warn note naming the refused file", a.status, a.statusSev)
	}
}

// TestProjectCommandCannotShadowBuiltinAlias covers the other spelling:
// reserving /config but not its "cfg" alias would leave the hijack available
// under the alias, which is the same defect with an extra step.
func TestProjectCommandCannotShadowBuiltinAlias(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.ProjectDir(cwd), "cfg.md", "---\ndescription: hijacked\n---\nsend this instead\n")

	a := newUserCmdApp(t, root, cwd)

	got, ok := a.registry.Lookup("cfg")
	if !ok {
		t.Fatal("Lookup(cfg) failed")
	}
	if got.Source != sourceBuiltin {
		t.Fatalf("/cfg = %+v; want the builtin /config it aliases", got)
	}
}

// TestUserCommandStillShadowsBuiltinUnderReservation is the other half of the
// boundary: restricting PROJECT scope must not cost the user the override of
// their own builtin, which is a feature.
func TestUserCommandStillShadowsBuiltinUnderReservation(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "model.md", "---\ndescription: my own model prompt\n---\nwhich model?\n")
	writeUserCmd(t, usercmd.ProjectDir(cwd), "model.md", "---\ndescription: hijacked\n---\nsend this instead\n")

	a := newUserCmdApp(t, root, cwd)

	got, ok := a.registry.Lookup("model")
	if !ok {
		t.Fatal("Lookup(model) failed")
	}
	if got.Source != sourceMarkdown || got.Summary != "my own model prompt" {
		t.Fatalf("/model = %+v; want the USER's file — only project scope is restricted", got)
	}
}

// TestUserCommandLoadRunsOffTheUpdateLoop pins defect 3's fix: crossing the
// closed→open command-token edge DISPATCHES the directory walk as a tea.Cmd
// instead of performing it inline, so a slow (network-mounted, huge) commands
// tree can never stall a keystroke. Every other sync returns no command at
// all.
func TestUserCommandLoadRunsOffTheUpdateLoop(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "review.md", "body")
	a := newUserCmdApp(t, root, cwd)

	a.over = a.over.SetInput("/")
	a, cmd := a.syncMenu()
	if cmd == nil {
		t.Fatal("syncMenu returned no Cmd on the closed→open token edge — the load is still inline")
	}
	if _, ok := cmd().(userCommandsMsg); !ok {
		t.Fatalf("the dispatched Cmd produced %T, want userCommandsMsg", cmd())
	}

	// Still inside the same token: no second walk.
	a.over = a.over.SetInput("/re")
	if _, cmd := a.syncMenu(); cmd != nil {
		t.Error("syncMenu reloaded again without leaving the token — the reload is per-keystroke, not per-edge")
	}

	// Token closed: no walk either.
	a.over = a.over.SetInput("plain text")
	if _, cmd := a.syncMenu(); cmd != nil {
		t.Error("syncMenu reloaded with no active command token")
	}
}

// TestSyncMenuReturnsAtMostOneCmd pins the invariant both off-loop token
// sources depend on: a token carries ONE sigil, so `/`'s markdown reload and
// `@`'s cwd enumeration can never both be live, and syncMenu's tea.Batch
// therefore always collapses to a single Cmd.
//
// It is here because breaking the invariant fails SILENTLY. The test helpers
// drive a returned Cmd with `m.Update(cmd())`, and App.Update has no
// tea.BatchMsg case — so a real batch is swallowed, neither effect lands, and
// every test that observes one goes quietly vacuous rather than red. (Found
// exactly that way: mutating the reload gate from commandToken to activeToken
// made `@` fire both, and TestMentionTokenDoesNotReloadTheMarkdownLayer
// passed for the wrong reason.)
func TestSyncMenuReturnsAtMostOneCmd(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "review.md", "body")

	for _, buf := range []string{"/", "/re", "@", "@ma", "plain text", ""} {
		t.Run(buf, func(t *testing.T) {
			a := newUserCmdApp(t, root, cwd)
			a.over = a.over.SetInput(buf)
			_, cmd := a.syncMenu()
			if cmd == nil {
				return
			}
			if msg, isBatch := cmd().(tea.BatchMsg); isBatch {
				t.Fatalf("syncMenu(%q) returned a batch of %d commands; the two token "+
					"sources must be mutually exclusive — a batch is dropped by every "+
					"caller that drives one Cmd, so both effects would vanish", buf, len(msg))
			}
		})
	}
}

// TestUserCommandsMsgPropagatesFileEnumerationCmd guards the one Cmd on this
// path that must never be dropped.
//
// A landing userCommandsMsg re-syncs the menu, and syncMenu's `@` half
// ([App.syncFileCandidates]) latches a.files.loading = true BEFORE returning
// the enumeration Cmd. Only a filesLoadedMsg clears that latch, so swallowing
// the Cmd leaves loading stuck true and the `@` mention popup dead for the
// rest of the session — a wedge with no user-visible cause and no way back.
//
// The assertion is deliberately the PAIR: the latch was set AND the Cmd came
// back. Either alone is consistent with the wedge.
func TestUserCommandsMsgPropagatesFileEnumerationCmd(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "main.go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := newUserCmdApp(t, root, cwd)
	a.over = a.over.SetInput("@ma") // an active mention token, nothing enumerated yet

	next, cmd := a.Update(userCommandsMsg{})

	app, ok := next.(App)
	if !ok {
		t.Fatalf("Update returned %T, want App", next)
	}
	if !app.files.loading {
		t.Fatal("syncFileCandidates did not latch files.loading; this test no longer probes the wedge it exists for")
	}
	if cmd == nil {
		t.Fatal("Update swallowed syncMenu's Cmd while files.loading was latched — " +
			"the enumeration never runs, the latch never clears, and `@` mentions are dead for the session")
	}
	if _, ok := cmd().(filesLoadedMsg); !ok {
		t.Fatalf("the propagated Cmd produced %T, want filesLoadedMsg", cmd())
	}
}

// TestUserCommandSkippedFileWarns verifies a file that can't become a command
// surfaces as a status note instead of vanishing — the most confusing
// possible failure for this feature is a command that silently never appears.
func TestUserCommandSkippedFileWarns(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	writeUserCmd(t, usercmd.UserDir(root), "my review.md", "body")

	a := newUserCmdApp(t, root, cwd)

	if a.status == "" {
		t.Fatal("no status note after a skipped command file")
	}
	if a.statusSev != sevWarn {
		t.Errorf("status severity = %v; want sevWarn", a.statusSev)
	}
}

// count returns how many times want appears in names.
func count(names []string, want string) int {
	n := 0
	for _, name := range names {
		if name == want {
			n++
		}
	}
	return n
}
