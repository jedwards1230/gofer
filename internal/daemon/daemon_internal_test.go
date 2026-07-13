package daemon

import (
	"context"
	"testing"
)

// TestServe_SetsReadHeaderTimeout asserts the http.Server Serve builds sets
// ReadHeaderTimeout to this package's readHeaderTimeout constant — this lives
// in the internal (package daemon) test file, rather than daemon_test, purely
// so it can read the unexported d.server field directly. sup is left nil: an
// already-cancelled ctx means Serve never accepts a connection (nothing ever
// dereferences it) before returning via the ctx.Done() shutdown path.
func TestServe_SetsReadHeaderTimeout(t *testing.T) {
	d := New(nil, Config{ListenAddr: "127.0.0.1:0"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Serve(ctx); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	if d.server == nil {
		t.Fatal("d.server is nil after Serve — did Serve return before building it?")
	}
	if d.server.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", d.server.ReadHeaderTimeout, readHeaderTimeout)
	}
}
