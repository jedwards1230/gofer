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
	"testing"
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
