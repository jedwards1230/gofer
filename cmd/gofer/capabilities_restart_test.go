package main

// capabilities_restart_test.go covers what happens to the DAEMON-BOUND closures
// on [tui.CommandEnv] when the stale-daemon banner's one-key restart
// (shift+R → [daemonbridge.Supervisor.RestartDaemon]) swaps the bridge's
// connection underneath them.
//
// The swap closes the connection the TUI started on — the reconstruction core
// owns the client's lifetime (see internal/daemonbridge's conn) — so a closure
// that captured that client at construction keeps calling a closed one forever
// after. It fails the way this whole area fails: quietly and plausibly. The
// roster beside it polls the replacement perfectly happily, while /mcp and
// /skills sit on UNKNOWN and /model's daemon re-read reports "could not ask"
// (both of which are also exactly what an unreachable daemon looks like), until
// the operator restarts the TUI process itself.
//
// Nothing here can be caught inside internal/tui: the TUI drives the closures it
// is handed and cannot see what they are bound to. It is a cmd/gofer wiring
// property, so these tests drive the production wiring — selectTUIBackend for
// bare `gofer`, attachCommandEnv for `gofer attach` — across a REAL restart, and
// assert the answers come from the replacement daemon rather than going away.

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
)

// constantClient adapts one already-dialed client to the supplier
// [daemonCapabilities]/[attachCommandEnv] take.
//
// It is for tests with NO bridge in play, where there is no connection swap to
// follow and the supplier is pure ceremony. Production must never build one:
// binding a daemon closure to a fixed client is precisely the bug the tests
// below exist for — see [daemonbridge.Supervisor.Client].
func constantClient(c *daemon.Client) func() *daemon.Client {
	return func() *daemon.Client { return c }
}

// stubDaemonRestart replaces the process-level restart step with fn for the rest
// of the test, restoring it afterwards.
//
// Everything AROUND it stays production code: the reconnect closure cmd/gofer
// built, its redial through [dialDaemon] against the same [daemonFlags], and the
// bridge's swap-and-close. Only the stop-the-process/start-a-replacement step is
// stubbed, because performing it for real would mean spawning an actual daemon
// process from a unit test. See [restartDaemonProcess]'s doc for why it is a var.
func stubDaemonRestart(t *testing.T, fn func(ctx context.Context, root string) error) {
	t.Helper()
	prev := restartDaemonProcess
	restartDaemonProcess = fn
	t.Cleanup(func() { restartDaemonProcess = prev })
}

// skillNames lists the loaded skill names in a capability answer, for
// assertions that care about WHICH daemon answered.
func skillNames(a capability.Answer) []string {
	out := make([]string, 0, len(a.Snapshot.Skills.Loaded))
	for _, s := range a.Snapshot.Skills.Loaded {
		out = append(out, s.Name)
	}
	return out
}

func hasSkill(a capability.Answer, name string) bool {
	for _, s := range a.Snapshot.Skills.Loaded {
		if s.Name == name {
			return true
		}
	}
	return false
}

// TestDaemonBackendCapabilitiesFollowTheRestartedDaemon is the bare-`gofer`
// regression, end to end through selectTUIBackend.
//
// Two daemons stand in for the one being replaced: each has a store root
// carrying a skill only IT can report, so the answer names which connection the
// closure actually used — not merely whether it got one. The stubbed restart
// step repoints the daemonFlags the reconnect closure already holds, which is
// what a real restart does (the replacement binds the same address; here two
// httptest servers cannot share one).
//
// Asserting Known alone would be weaker but still enough to fail before the
// fix: the pre-restart client is CLOSED by the swap, so a captured one reports
// UNKNOWN. The skill assertion additionally rules out an answer that is merely
// well-formed.
func TestDaemonBackendCapabilitiesFollowTheRestartedDaemon(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	writeBackendSkill(t, filepath.Join(rootA, "skills", "daemon-a-only"), "daemon-a-only")
	writeBackendSkill(t, filepath.Join(rootB, "skills", "daemon-b-only"), "daemon-b-only")
	addrA := testDaemonAt(t, testDaemonSpec{root: rootA}, fauxProvider)
	addrB := testDaemonAt(t, testDaemonSpec{root: rootB}, fauxProvider)

	ctx := context.Background()
	df := &daemonFlags{addr: addrA}
	backend, err := selectTUIBackend(ctx, df, t.TempDir(), t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()
	if !backend.env.DaemonBacked {
		t.Fatal("test premise broken: expected the daemon backend")
	}

	before, err := backend.env.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities before restart: %v", err)
	}
	if !hasSkill(before, "daemon-a-only") {
		t.Fatalf("test premise broken: the pre-restart daemon must report its own root's skill, got %v", skillNames(before))
	}

	// The replacement: the reconnect closure selectTUIBackend built redials
	// through this same df, so repointing it lands the new connection on daemon B.
	stubDaemonRestart(t, func(context.Context, string) error {
		df.addr = addrB
		return nil
	})
	if err := backend.sup.RestartDaemon(ctx); err != nil {
		t.Fatalf("RestartDaemon: %v", err)
	}

	after, err := backend.env.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities after restart: %v — the closure is still bound to the connection the restart CLOSED, "+
			"so /mcp and /skills render UNKNOWN until the TUI process itself restarts", err)
	}
	if !after.Known {
		t.Fatalf("after a successful restart the panels must have an answer again, got %+v", after)
	}
	if !hasSkill(after, "daemon-b-only") {
		t.Errorf("the capability closure did not follow the restart onto the replacement daemon: got %v, want it to include daemon-b-only", skillNames(after))
	}
	if hasSkill(after, "daemon-a-only") {
		t.Errorf("the capability closure is still reading the REPLACED daemon: got %v", skillNames(after))
	}
}

// TestDaemonBackendDefaultModelFollowsTheRestartedDaemon is the same regression
// for [tui.CommandEnv.DaemonDefaultModel] — the /model panel's re-read of the
// daemon's own default after a write (gofer#162), which was bound to the
// pre-restart connection in the same breath as Capabilities.
//
// The two daemons advertise different defaults on gofer/hello, so the answer
// identifies which connection was used. A captured client cannot merely go
// stale here: it is closed, so the read fails outright and the panel falls back
// to its hedged "could not ask the daemon" wording.
func TestDaemonBackendDefaultModelFollowsTheRestartedDaemon(t *testing.T) {
	addrA := testDaemonAt(t, testDaemonSpec{defaultModel: "model-a"}, fauxProvider)
	addrB := testDaemonAt(t, testDaemonSpec{defaultModel: "model-b"}, fauxProvider)

	ctx := context.Background()
	df := &daemonFlags{addr: addrA}
	backend, err := selectTUIBackend(ctx, df, t.TempDir(), t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()
	if backend.env.DaemonDefaultModel == nil {
		t.Fatal("the daemon backend supplied no DaemonDefaultModel closure — /model can never re-read the daemon's default (gofer#162)")
	}

	before, err := backend.env.DaemonDefaultModel(ctx)
	if err != nil {
		t.Fatalf("DaemonDefaultModel before restart: %v", err)
	}
	if before != "model-a" {
		t.Fatalf("test premise broken: DaemonDefaultModel = %q, want the pre-restart daemon's %q", before, "model-a")
	}

	stubDaemonRestart(t, func(context.Context, string) error {
		df.addr = addrB
		return nil
	})
	if err := backend.sup.RestartDaemon(ctx); err != nil {
		t.Fatalf("RestartDaemon: %v", err)
	}

	after, err := backend.env.DaemonDefaultModel(ctx)
	if err != nil {
		t.Fatalf("DaemonDefaultModel after restart: %v — the closure is still bound to the connection the restart CLOSED, "+
			"so /model reports the daemon cannot answer until the TUI process itself restarts", err)
	}
	if after != "model-b" {
		t.Errorf("DaemonDefaultModel = %q after the restart, want the REPLACEMENT daemon's %q", after, "model-b")
	}
}

// TestAttachCapabilitiesFollowTheRestartedDaemon is the `gofer attach` twin.
//
// attach reaches its TUI only behind an interactive-terminal check (see
// runAttach), so this drives the binder attach.go calls — attachCommandEnv,
// pinned as attach's one env builder by
// TestAttachDoesNotUseTheSharedEnvBuilderDirectly — over the same bridge
// construction, with the same restart underneath it.
func TestAttachCapabilitiesFollowTheRestartedDaemon(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	writeBackendSkill(t, filepath.Join(rootA, "skills", "attach-a-only"), "attach-a-only")
	writeBackendSkill(t, filepath.Join(rootB, "skills", "attach-b-only"), "attach-b-only")
	addrA := testDaemonAt(t, testDaemonSpec{root: rootA}, fauxProvider)
	addrB := testDaemonAt(t, testDaemonSpec{root: rootB}, fauxProvider)

	ctx := context.Background()
	c, err := daemon.Dial(ctx, addrA, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	b := daemonbridge.New(c, daemonbridge.WithReconnect(func(ctx context.Context) (*daemon.Client, error) {
		return daemon.Dial(ctx, addrB, "")
	}))
	defer func() { _ = b.Close() }()

	env := attachCommandEnv(b.Client, t.TempDir(), t.TempDir())
	if env.Capabilities == nil {
		t.Fatal("gofer attach supplied no Capabilities closure — /mcp and /skills are permanently UNKNOWN there")
	}
	before, err := env.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities before restart: %v", err)
	}
	if !hasSkill(before, "attach-a-only") {
		t.Fatalf("test premise broken: the pre-restart daemon must report its own root's skill, got %v", skillNames(before))
	}

	if err := b.RestartDaemon(ctx); err != nil {
		t.Fatalf("RestartDaemon: %v", err)
	}

	after, err := env.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities after restart: %v — attach's closure is still bound to the connection the restart CLOSED", err)
	}
	if !after.Known {
		t.Fatalf("after a successful restart the panels must have an answer again, got %+v", after)
	}
	if !hasSkill(after, "attach-b-only") {
		t.Errorf("attach's capability closure did not follow the restart onto the replacement daemon: got %v, want it to include attach-b-only", skillNames(after))
	}
	if hasSkill(after, "attach-a-only") {
		t.Errorf("attach's capability closure is still reading the REPLACED daemon: got %v", skillNames(after))
	}
}
