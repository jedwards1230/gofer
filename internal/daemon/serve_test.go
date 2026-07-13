package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestServe_RejectsNonLoopbackWithoutToken asserts Serve refuses to bind at
// all — no listener is ever opened — when ListenAddr is non-loopback and
// BearerToken is empty. Because ValidateListen runs before Serve touches the
// network, this needs no real network access and no context cancellation:
// Serve returns the validation error immediately.
func TestServe_RejectsNonLoopbackWithoutToken(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	d := daemon.New(sup, daemon.Config{ListenAddr: "192.168.1.50:7333"})

	err := d.Serve(context.Background())
	if err == nil {
		t.Fatal("Serve: want an error for a non-loopback bind with no token, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to bind") {
		t.Errorf("Serve error = %q, want it to mention refusing to bind", err.Error())
	}
}

// TestServe_AcceptsValidatedConfigs asserts Serve does NOT reject a loopback
// bind with no token, nor a non-loopback bind that does carry a token — both
// pass ValidateListen and reach the real listen/shutdown path. Each case
// binds ":0" (an OS-assigned ephemeral port on loopback-reachable interfaces
// only) and starts with an already-cancelled ctx, so this never depends on
// external connectivity or timing — same footprint as httptest.NewServer's
// own internal use of a loopback listener.
func TestServe_AcceptsValidatedConfigs(t *testing.T) {
	cases := []struct {
		name  string
		addr  string
		token string
	}{
		{"loopback, no token", "127.0.0.1:0", ""},
		{"non-loopback (bind-all), with token", "0.0.0.0:0", "some-token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sup := newTestSupervisor(t, fauxProvider)
			d := daemon.New(sup, daemon.Config{ListenAddr: tc.addr, BearerToken: tc.token})

			// An already-cancelled ctx makes Serve's own select deterministic:
			// the ctx.Done() branch is guaranteed ready by the time Serve's
			// select runs, and the ListenAndServe goroutine only ever sends on
			// errCh on a listen failure (impossible for a free ephemeral
			// loopback/bind-all port) or never at all while blocked in Accept.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			errCh := make(chan error, 1)
			go func() { errCh <- d.Serve(ctx) }()

			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("Serve = %v, want nil (validation passed, clean shutdown)", err)
				}
			case <-time.After(defaultWait):
				t.Fatal("Serve did not return")
			}
		})
	}
}
