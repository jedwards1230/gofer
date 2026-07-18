package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// writeTestEndpoint advertises a plausible endpoint for sessionID, exactly as a
// starting worker does just before it serves.
func writeTestEndpoint(t *testing.T, sessionID string) daemon.WorkerEndpoint {
	t.Helper()
	ep := daemon.WorkerEndpoint{
		Addr:          "unix:///tmp/" + sessionID + ".sock",
		PID:           os.Getpid(),
		BinaryVersion: "test",
		WireVersion:   daemon.WireVersion,
		StartedAt:     time.Now(),
	}
	if err := daemon.WriteWorkerEndpoint(sessionID, ep); err != nil {
		t.Fatalf("WriteWorkerEndpoint(%s): %v", sessionID, err)
	}
	return ep
}

// endpointPublisher pre-stages everything [daemon.WriteWorkerEndpoint] would do
// for sessionID EXCEPT the final rename, and returns a publish func that makes
// the endpoint appear atomically in microseconds. The timing test needs the
// publish instant to be the only thing it measures against: the production
// writer fsyncs its temp file, which costs ~10ms on some filesystems and would
// otherwise be indistinguishable from discovery lag.
func endpointPublisher(t *testing.T, sessionID string) func() {
	t.Helper()
	path, err := daemon.WorkerEndpointPath(sessionID)
	if err != nil {
		t.Fatalf("WorkerEndpointPath: %v", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir workers dir: %v", err)
	}
	b, err := json.Marshal(daemon.WorkerEndpoint{
		Addr:        "unix:///tmp/" + sessionID + ".sock",
		PID:         os.Getpid(),
		WireVersion: daemon.WireVersion,
		StartedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("marshal endpoint: %v", err)
	}
	staged := filepath.Join(dir, "staged-"+sessionID)
	if err := os.WriteFile(staged, b, 0o600); err != nil {
		t.Fatalf("stage endpoint: %v", err)
	}
	return func() { _ = os.Rename(staged, path) }
}

// TestWaitForWorkerEndpointFastPath proves the no-wait case: an endpoint already
// on disk is returned by the FIRST read, before any sleep — the discovery loop
// never blocks on its poll timer, its ctx, or the worker's wait channel.
func TestWaitForWorkerEndpointFastPath(t *testing.T) {
	shortRuntimeDir(t)
	sessionID := uuid.Must(uuid.NewV7()).String()
	want := writeTestEndpoint(t, sessionID)

	// A pre-fired wait channel and an ALREADY-CANCELLED ctx: if the loop reached
	// its select at all, one of them would win and this would fail. Reaching the
	// select is exactly what the fast path must not do.
	wait := make(chan error, 1)
	wait <- errors.New("worker exited")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ep, exited, err := waitForWorkerEndpoint(ctx, sessionID, wait)
	if err != nil {
		t.Fatalf("waitForWorkerEndpoint: %v", err)
	}
	if exited {
		t.Fatal("exited = true on the fast path; the wait channel must not be consumed")
	}
	if ep.Addr != want.Addr || ep.PID != want.PID {
		t.Fatalf("endpoint = %+v, want addr %q pid %d", ep, want.Addr, want.PID)
	}
	if len(wait) != 1 {
		t.Fatal("the worker's wait result was consumed on the fast path")
	}
}

// TestWaitForWorkerEndpointAppearsQuickly is the latency assertion for the
// tight-then-backoff cadence (M6 §10's startup-latency cost): an endpoint
// published ~1ms after the spawn — the common fast-start case — is discovered in
// single-digit milliseconds. The cadence this replaces (a fixed 15ms ticker)
// could not pass it: its first read misses, and the next one is a full 15ms
// later, so it always returned at ≥15ms. That gap is exactly what the bound
// below guards.
func TestWaitForWorkerEndpointAppearsQuickly(t *testing.T) {
	shortRuntimeDir(t)
	sessionID := uuid.Must(uuid.NewV7()).String()
	publish := endpointPublisher(t, sessionID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wait := make(chan error, 1)

	go func() {
		time.Sleep(endpointPollInitial)
		publish()
	}()

	start := time.Now()
	ep, exited, err := waitForWorkerEndpoint(ctx, sessionID, wait)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waitForWorkerEndpoint: %v", err)
	}
	if exited {
		t.Fatal("exited = true, want false")
	}
	if ep.Addr == "" {
		t.Fatal("endpoint has no addr")
	}
	// 12ms: under the old 15ms first tick, well over the ~3ms the backoff
	// schedule (1ms, 2ms, 4ms…) actually needs for a 1ms publish.
	if limit := 12 * time.Millisecond; elapsed > limit {
		t.Fatalf("discovery took %v, want < %v (backoff cadence regressed to a fixed interval?)", elapsed, limit)
	}
}

// TestWaitForWorkerEndpointOutcomes covers the three non-success terminal
// outcomes, which the latency change must leave EXACTLY as they were: ctx
// expiry, the worker exiting before it advertises (with and without an exit
// error), and a genuine read/parse failure that is not "not written yet".
func TestWaitForWorkerEndpointOutcomes(t *testing.T) {
	exitErr := errors.New("exit status 2")

	tests := []struct {
		name string
		// setup optionally seeds on-disk state for sessionID.
		setup func(t *testing.T, sessionID string)
		// wait seeds the worker's exit channel.
		wait       func() chan error
		ctx        func(t *testing.T) (context.Context, context.CancelFunc)
		wantExited bool
		wantErrIs  error
		wantErrHas string
	}{
		{
			name: "ctx expires with no endpoint",
			wait: func() chan error { return make(chan error, 1) },
			ctx: func(*testing.T) (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
			wantExited: false,
			wantErrIs:  context.DeadlineExceeded,
		},
		{
			name: "ctx already cancelled",
			wait: func() chan error { return make(chan error, 1) },
			ctx: func(*testing.T) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantExited: false,
			wantErrIs:  context.Canceled,
		},
		{
			name: "worker exits with an error before advertising",
			wait: func() chan error {
				ch := make(chan error, 1)
				ch <- exitErr
				return ch
			},
			ctx: func(*testing.T) (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 10*time.Second)
			},
			wantExited: true,
			wantErrIs:  exitErr,
			wantErrHas: "worker exited before advertising its endpoint",
		},
		{
			name: "worker exits cleanly before advertising",
			wait: func() chan error {
				ch := make(chan error, 1)
				ch <- nil
				return ch
			},
			ctx: func(*testing.T) (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 10*time.Second)
			},
			wantExited: true,
			wantErrHas: "worker exited before advertising its endpoint",
		},
		{
			name: "unparseable endpoint file is a hard failure",
			setup: func(t *testing.T, sessionID string) {
				t.Helper()
				path, err := daemon.WorkerEndpointPath(sessionID)
				if err != nil {
					t.Fatalf("WorkerEndpointPath: %v", err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("mkdir workers dir: %v", err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
					t.Fatalf("write corrupt endpoint: %v", err)
				}
			},
			// A pre-fired wait channel that must NOT be consumed: the read error
			// short-circuits before the select, so the caller still owns the drain.
			wait: func() chan error {
				ch := make(chan error, 1)
				ch <- nil
				return ch
			},
			ctx: func(*testing.T) (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 10*time.Second)
			},
			wantExited: false,
			wantErrHas: "read worker endpoint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortRuntimeDir(t)
			sessionID := uuid.Must(uuid.NewV7()).String()
			if tc.setup != nil {
				tc.setup(t, sessionID)
			}
			ctx, cancel := tc.ctx(t)
			defer cancel()

			_, exited, err := waitForWorkerEndpoint(ctx, sessionID, tc.wait())
			if err == nil {
				t.Fatal("waitForWorkerEndpoint returned no error, want one")
			}
			if exited != tc.wantExited {
				t.Fatalf("exited = %v, want %v", exited, tc.wantExited)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErrIs)
			}
			if tc.wantErrHas != "" && !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.wantErrHas)
			}
		})
	}
}
