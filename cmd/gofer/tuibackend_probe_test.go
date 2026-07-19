package main

// tuibackend_probe_test.go pins the process wiring behind issue #162's header
// refresh: the daemon-backed TUI is handed a CommandEnv.DaemonDefaultModel
// closure that re-reads the ATTACHED daemon's current default off gofer/hello,
// and the local backend is handed none.
//
// The TUI's own behavior on top of that closure is covered in internal/tui
// (model_select_probe_test.go). What can only be asserted HERE is that
// production actually supplies it — a closure nothing wires up is a feature
// that silently does not exist, and no TUI test would notice.

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestDaemonBackendSuppliesALiveDefaultModelProbe drives the real
// selectTUIBackend against a real in-process daemon and calls the closure it
// installed, asserting it answers with that daemon's own default.
//
// Deleting the env.DaemonDefaultModel assignment in selectTUIBackend fails
// this test — which is the point, since the header refresh degrades silently
// to the pre-#162 hedged behavior without it rather than breaking loudly.
func TestDaemonBackendSuppliesALiveDefaultModelProbe(t *testing.T) {
	// testDaemon configures DefaultModel "faux". Nothing on this side could
	// recompute that value, so seeing it proves the probe reached the daemon.
	addr := testDaemon(t, "", fauxProvider)
	df := &daemonFlags{addr: addr}

	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), "", io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if !backend.env.DaemonBacked {
		t.Fatal("test premise broken: expected the daemon backend")
	}
	if backend.env.DaemonDefaultModel == nil {
		t.Fatal("daemon backend supplied no DaemonDefaultModel probe — the TUI header can never refresh (issue #162)")
	}
	model, err := backend.env.DaemonDefaultModel(context.Background())
	if err != nil {
		t.Fatalf("DaemonDefaultModel: %v", err)
	}
	if model != "faux" {
		t.Errorf("DaemonDefaultModel = %q, want the daemon's own default %q", model, "faux")
	}
}

// TestLocalBackendSuppliesNoDefaultModelProbe is the negative half. There is no
// daemon to ask on the local path, and the TUI updates its header directly
// there, so a probe would be both meaningless and a latent way for a local TUI
// to start making network calls.
func TestLocalBackendSuppliesNoDefaultModelProbe(t *testing.T) {
	df := &daemonFlags{addr: closedDaemonAddr}
	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if backend.env.DaemonBacked {
		t.Fatal("test premise broken: expected the local backend")
	}
	if backend.env.DaemonDefaultModel != nil {
		t.Error("local backend supplied a DaemonDefaultModel probe; there is no daemon to ask")
	}
}

// TestDaemonDefaultModelErrTreatsUnsupportedHelloAsUnknown pins the
// older-daemon fallback the TUI depends on. A daemon predating gofer/hello
// replies method-not-found, which daemon.Client maps to ErrHelloUnsupported.
// That is a permanent "this daemon cannot answer", not a failure: reported as
// an empty model with NO error, it leaves the TUI on its hedged, no-probe
// wording instead of showing the user a connection error they cannot act on.
//
// Asserted on a closure that returns the sentinel rather than by standing up a
// pre-hello daemon, which no longer exists to build.
func TestDaemonDefaultModelErrTreatsUnsupportedHelloAsUnknown(t *testing.T) {
	// The exact wrapping daemon.Client.Hello performs (%w around the sentinel).
	wrapped := errors.Join(daemon.ErrHelloUnsupported, errors.New("rpc: method not found"))
	if !errors.Is(wrapped, daemon.ErrHelloUnsupported) {
		t.Fatal("test premise broken: the fixture error does not match the sentinel")
	}

	model, err := classifyHelloDefault("", wrapped)
	if err != nil {
		t.Errorf("an unsupported gofer/hello must not surface as an error, got %v", err)
	}
	if model != "" {
		t.Errorf("model = %q, want empty (unknown)", model)
	}

	// Any OTHER failure is still an error, so a genuine transport problem is
	// not silently indistinguishable from an old daemon at this layer.
	boom := errors.New("dial tcp: connection refused")
	if _, err := classifyHelloDefault("", boom); !errors.Is(err, boom) {
		t.Errorf("a transport failure must be returned, got %v", err)
	}
}
