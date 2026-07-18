package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemonbridge"
)

// TestSelectTUIBackend_NoDaemonFallsBackLocal covers the fallback half of
// the daemon-preference seam: an unreachable address (the closed-port
// convention this package's other daemon tests use) yields the local
// in-process backend, functional on its own (a Roster call against it
// succeeds with an empty roster from a fresh store), with no bubbletea
// involved.
func TestSelectTUIBackend_NoDaemonFallsBackLocal(t *testing.T) {
	root := t.TempDir()
	df := &daemonFlags{addr: "127.0.0.1:1"}

	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), root, io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if !strings.Contains(backend.label, "local") {
		t.Errorf("label = %q, want it to mention the local backend", backend.label)
	}
	if _, ok := backend.sup.(*daemonbridge.Supervisor); ok {
		t.Error("backend.sup is a *daemonbridge.Supervisor, want the local tuibridge one")
	}
	roster, err := backend.sup.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(roster) != 0 {
		t.Errorf("Roster = %+v, want empty (fresh store)", roster)
	}
}

// TestSelectTUIBackend_DaemonReachablePrefersDaemon covers the preferred
// half: a reachable daemon wins over the local path, and the returned
// backend is genuinely driving it (a Roster call round-trips over the real
// connection).
func TestSelectTUIBackend_DaemonReachablePrefersDaemon(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	df := &daemonFlags{addr: addr}

	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), "", io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if !strings.Contains(backend.label, "daemon") {
		t.Errorf("label = %q, want it to mention the daemon backend", backend.label)
	}
	if _, ok := backend.sup.(*daemonbridge.Supervisor); !ok {
		t.Errorf("backend.sup = %T, want *daemonbridge.Supervisor", backend.sup)
	}
	if _, err := backend.sup.Roster(context.Background()); err != nil {
		t.Errorf("Roster: %v", err)
	}
}

// TestSelectTUIBackend_UnauthorizedIsHardError covers the third case: a
// daemon IS listening but rejects the token — this must be a hard error, not
// a silent fallback to the local roster (see selectTUIBackend's doc).
func TestSelectTUIBackend_UnauthorizedIsHardError(t *testing.T) {
	addr := testDaemon(t, "the-real-token", fauxProvider)
	df := &daemonFlags{addr: addr, token: "wrong"}

	_, err := selectTUIBackend(context.Background(), df, t.TempDir(), "", io.Discard)
	if err == nil {
		t.Fatal("selectTUIBackend with wrong token: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("err = %v, want it to name the auth problem", err)
	}
}
