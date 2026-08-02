package main

// capabilities_backend_test.go pins the PROCESS WIRING behind gofer#303's
// /mcp and /skills panels: which backend supplies [tui.CommandEnv.Capabilities]
// and, critically, which source it reads.
//
// The TUI's behavior on top of that closure is covered in internal/tui. What
// can only be asserted HERE is that production supplies it at all — a closure
// nothing wires up is a feature that silently does not exist — and that the
// daemon-backed one reads the WIRE rather than this process's own filesystem.
// No test inside internal/tui can see that difference: both would hand it a
// perfectly well-formed snapshot.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestDaemonBackendReadsCapabilitiesOverTheWireNotLocally is the process-level
// half of the anti-lie guard.
//
// The client's store root carries a skill the DAEMON's root does not. The
// daemon resolves `<its own root>/skills`, so a wire answer cannot contain it;
// a local read would contain it immediately. Asserting its absence is
// therefore a direct test of which source the closure consulted — not of
// whether it returned something plausible.
func TestDaemonBackendReadsCapabilitiesOverTheWireNotLocally(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)

	clientRoot := t.TempDir()
	writeBackendSkill(t, filepath.Join(clientRoot, "skills", "client-root-only"), "client-root-only")

	df := &daemonFlags{addr: addr}
	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), clientRoot, io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if !backend.env.DaemonBacked {
		t.Fatal("test premise broken: expected the daemon backend")
	}
	if backend.env.Capabilities == nil {
		t.Fatal("daemon backend supplied no Capabilities closure — /mcp and /skills can never render (gofer#303)")
	}

	answer, err := backend.env.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !answer.Known {
		t.Fatal("a live daemon implementing gofer/capabilities must answer Known")
	}
	for _, s := range answer.Snapshot.Skills.Loaded {
		if s.Name == "client-root-only" {
			t.Fatalf("the daemon-backed closure read THIS process's store root: %+v", answer.Snapshot.Skills.Loaded)
		}
	}
	for _, dir := range answer.Snapshot.Skills.Directories {
		if dir == filepath.Join(clientRoot, "skills") {
			t.Errorf("the daemon reported this client's store root %q as its own discovery directory", dir)
		}
	}
}

// TestLocalBackendReadsItsOwnSupervisor is the positive twin: on the local
// path this process DOES own the supervisor, so its store root is exactly the
// right thing to report.
func TestLocalBackendReadsItsOwnSupervisor(t *testing.T) {
	root := t.TempDir()
	writeBackendSkill(t, filepath.Join(root, "skills", "local-root-skill"), "local-root-skill")

	df := &daemonFlags{addr: closedDaemonAddr}
	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), root, io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if backend.env.DaemonBacked {
		t.Fatal("test premise broken: expected the local backend")
	}
	if backend.env.Capabilities == nil {
		t.Fatal("local backend supplied no Capabilities closure — /mcp and /skills can never render (gofer#303)")
	}

	answer, err := backend.env.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !answer.Known {
		t.Fatal("the in-process supervisor always has an answer")
	}
	found := false
	for _, s := range answer.Snapshot.Skills.Loaded {
		if s.Name == "local-root-skill" {
			found = true
		}
	}
	if !found {
		t.Errorf("the local closure must read this process's own store root, got %+v", answer.Snapshot.Skills.Loaded)
	}
}

// TestDaemonCapabilitiesFailureStaysUnknownAndReadsNothingLocally covers the
// branch TestDaemonBackendReadsCapabilitiesOverTheWireNotLocally cannot: what
// happens when the wire CANNOT answer.
//
// A live daemon that answers correctly proves the closure did not ALWAYS read
// locally. It says nothing about the far more tempting bug — a fallback guarded
// on "the daemon had no answer", which is inert on the happy path and fires
// exactly when a user is least able to notice. So this test breaks the
// connection first, and plants a skill under the cwd that any local read would
// pick up.
//
// The assertion is that unknown STAYS unknown. Substituting a local snapshot
// here would render a complete, confident panel about this machine while the
// TUI says it is attached to a daemon.
func TestDaemonCapabilitiesFailureStaysUnknownAndReadsNothingLocally(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	c, err := daemon.Dial(context.Background(), addr, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Break it: every subsequent call fails at the transport, which is the
	// generic "no answer" this closure must not paper over.
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cwd := t.TempDir()
	writeBackendSkill(t, filepath.Join(cwd, ".gofer", "skills", "cwd-local-skill"), "cwd-local-skill")

	answer, err := daemonCapabilities(constantClient(c), cwd)(context.Background())
	if err == nil {
		t.Error("a broken connection must surface its error to the closure's caller")
	}
	if answer.Known {
		t.Fatalf("a failed wire read must stay UNKNOWN, got %+v", answer)
	}
	if len(answer.Snapshot.Skills.Loaded) != 0 || len(answer.Snapshot.MCP.Servers) != 0 {
		t.Fatalf("the daemon closure fell back to a LOCAL read: %+v", answer.Snapshot)
	}
}

// TestSharedCommandEnvBuilderLeavesCapabilitiesNil pins the rule that makes the
// two branches above safe to reason about: the builder BOTH backends share
// installs nothing.
//
// Setting it there would be the single easiest way to reintroduce the bug —
// the daemon branch would silently inherit a local reader, and every panel
// would keep rendering something entirely plausible.
func TestSharedCommandEnvBuilderLeavesCapabilitiesNil(t *testing.T) {
	env := buildCommandEnv(t.TempDir(), t.TempDir())
	if env.Capabilities != nil {
		t.Error("buildCommandEnv must leave Capabilities nil — only a specific backend may bind it (gofer#303)")
	}
}

// TestAttachWiresDaemonCapabilities is the regression for `gofer attach`
// rendering /mcp and /skills as permanently UNKNOWN.
//
// runAttach built its env with the shared buildCommandEnv, which by contract
// leaves Capabilities nil — so on the one entrypoint CLAUDE.md calls the
// daemon-attached TUI, both tabs said UNKNOWN against a daemon that could
// answer perfectly. It failed SAFE rather than lying, which is exactly why
// nothing caught it: an unwired closure and an unreachable daemon look
// identical on screen.
func TestAttachWiresDaemonCapabilities(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	c, err := daemon.Dial(context.Background(), addr, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	clientRoot := t.TempDir()
	writeBackendSkill(t, filepath.Join(clientRoot, "skills", "attach-client-only"), "attach-client-only")

	env := attachCommandEnv(constantClient(c), clientRoot, t.TempDir())
	if env.Capabilities == nil {
		t.Fatal("gofer attach supplied no Capabilities closure — /mcp and /skills are permanently UNKNOWN there")
	}
	answer, err := env.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !answer.Known {
		t.Fatal("a live daemon implementing gofer/capabilities must answer Known on the attach path too")
	}
	// Same wire-not-local guard the bare-`gofer` path gets: the closure must be
	// bound to the connection, not to this process's store root.
	for _, s := range answer.Snapshot.Skills.Loaded {
		if s.Name == "attach-client-only" {
			t.Fatalf("the attach closure read THIS process's store root: %+v", answer.Snapshot.Skills.Loaded)
		}
	}
}

// TestAttachDoesNotUseTheSharedEnvBuilderDirectly guards the WIRING, which the
// behavioral test above cannot reach: runAttach requires an interactive
// terminal and returns before building an env under test.
//
// It is a source-level assertion on purpose, and narrow: the regression is
// someone "simplifying" attach.go back to the shared builder, which compiles,
// passes every other test, and silently turns both tabs off. Reading the one
// call site is a cheaper and more direct guard than a TTY harness.
func TestAttachDoesNotUseTheSharedEnvBuilderDirectly(t *testing.T) {
	src, err := os.ReadFile("attach.go")
	if err != nil {
		t.Fatalf("read attach.go: %v", err)
	}
	if strings.Contains(string(src), "buildCommandEnv(") {
		t.Error("attach.go must build its env via attachCommandEnv — the shared buildCommandEnv leaves Capabilities nil, " +
			"which renders /mcp and /skills permanently UNKNOWN on the daemon-attached TUI")
	}
	if !strings.Contains(string(src), "attachCommandEnv(") {
		t.Error("attach.go no longer calls attachCommandEnv")
	}
}

func writeBackendSkill(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "---\nname: " + name + "\ndescription: a skill reachable only from one store root\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s/SKILL.md: %v", dir, err)
	}
}
