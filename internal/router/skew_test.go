package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestClassifySkew pins the M6 §6 version decision branch by branch. It is the
// primary coverage for the policy: every routing consequence downstream is a
// pure function of this class, so the process-level tests only have to prove the
// class reaches the handle.
func TestClassifySkew(t *testing.T) {
	const (
		routerWire = 7
		v1         = "v1.2.3"
		v2         = "v1.3.0"
	)
	tests := []struct {
		name         string
		routerBinary string
		workerBinary string
		workerWire   int
		want         skewClass
	}{
		{
			name:         "identical binary and wire",
			routerBinary: v1, workerBinary: v1, workerWire: routerWire,
			want: skewNone,
		},
		{
			name:         "both sides unidentified is not skew",
			routerBinary: "", workerBinary: "", workerWire: routerWire,
			want: skewNone,
		},
		{
			name:         "older worker binary on a matching wire",
			routerBinary: v2, workerBinary: v1, workerWire: routerWire,
			want: skewBinary,
		},
		{
			name:         "newer worker binary on a matching wire",
			routerBinary: v1, workerBinary: v2, workerWire: routerWire,
			want: skewBinary,
		},
		{
			// A dirty build IS a different binary from its base commit, so the
			// comparison must stay exact rather than normalizing the suffix away.
			name:         "dirty build differs from its base commit",
			routerBinary: "dev-abc123def456", workerBinary: "dev-abc123def456-dirty", workerWire: routerWire,
			want: skewBinary,
		},
		{
			// Exact match, not N-1: an adjacent release is still skew.
			name:         "adjacent versions are still binary skew (exact match, not N-1)",
			routerBinary: "v1.2.4", workerBinary: "v1.2.3", workerWire: routerWire,
			want: skewBinary,
		},
		{
			name:         "worker predating version reporting is unknown, not skew-free",
			routerBinary: v1, workerBinary: "", workerWire: routerWire,
			want: skewUnknown,
		},
		{
			name:         "unidentified router against an identified worker is unknown",
			routerBinary: "", workerBinary: v1, workerWire: routerWire,
			want: skewUnknown,
		},
		{
			name:         "older wire dominates an identical binary",
			routerBinary: v1, workerBinary: v1, workerWire: routerWire - 1,
			want: skewWire,
		},
		{
			name:         "newer wire dominates an identical binary",
			routerBinary: v1, workerBinary: v1, workerWire: routerWire + 1,
			want: skewWire,
		},
		{
			name:         "wire dominates binary skew too",
			routerBinary: v2, workerBinary: v1, workerWire: routerWire + 1,
			want: skewWire,
		},
		{
			name:         "wire dominates an unknown binary too",
			routerBinary: v1, workerBinary: "", workerWire: routerWire + 1,
			want: skewWire,
		},
		{
			// A worker that reports no wire version at all (a zero value from an
			// older handshake shape) is a WIRE mismatch, not an unknown binary:
			// the router cannot confirm the protocol, so it takes the strict path.
			name:         "absent wire version is treated as a wire mismatch",
			routerBinary: v1, workerBinary: v1, workerWire: 0,
			want: skewWire,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySkew(tc.routerBinary, tc.workerBinary, routerWire, tc.workerWire)
			if got != tc.want {
				t.Errorf("classifySkew(%q, %q, %d, %d) = %v, want %v",
					tc.routerBinary, tc.workerBinary, routerWire, tc.workerWire, got, tc.want)
			}
		})
	}
}

// TestSkewClassRefusesNewWork is the policy assertion that matters most: ONLY a
// wire mismatch refuses new work. Binary and unknown skew must stay routable, or
// a daemon upgrade would strand every live session (Resume cannot yet spawn a
// replacement worker — that is Phase 4).
func TestSkewClassRefusesNewWork(t *testing.T) {
	tests := []struct {
		class skewClass
		want  bool
	}{
		{skewNone, false},
		{skewBinary, false},
		{skewUnknown, false},
		{skewWire, true},
	}
	for _, tc := range tests {
		t.Run(tc.class.String(), func(t *testing.T) {
			if got := tc.class.refusesNewWork(); got != tc.want {
				t.Errorf("%v.refusesNewWork() = %v, want %v", tc.class, got, tc.want)
			}
		})
	}
}

// TestRefuseNewWorkError proves the refusal's shape: a typed, errors.Is-able
// [ErrWorkerSkewed] naming both wire versions, and nothing at all for the
// classes that stay routable.
func TestRefuseNewWorkError(t *testing.T) {
	skewed := &workerHandle{skew: skewWire, wireVersion: daemon.WireVersion + 1}
	err := skewed.refuseNewWork("send")
	if !errors.Is(err, ErrWorkerSkewed) {
		t.Fatalf("refuseNewWork on wire skew = %v, want errors.Is ErrWorkerSkewed", err)
	}
	for _, class := range []skewClass{skewNone, skewBinary, skewUnknown} {
		h := &workerHandle{skew: class, binaryVersion: "old"}
		if err := h.refuseNewWork("send"); err != nil {
			t.Errorf("refuseNewWork on %v = %v, want nil (only wire skew refuses)", class, err)
		}
	}
}

// TestClassifyWorkerUsesTheRoutersOwnVersions checks the handle-creation seam
// both Create and adoptWorker share: it must compare the worker's HANDSHAKE
// against the router's configured Version and the router's own wire constant.
func TestClassifyWorkerUsesTheRoutersOwnVersions(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	sup, err := New(Config{Root: root, Version: "router-build", NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	tests := []struct {
		name  string
		hello daemon.HelloResult
		want  skewClass
	}{
		{"same build", daemon.HelloResult{BinaryVersion: "router-build", WireVersion: daemon.WireVersion}, skewNone},
		{"older build", daemon.HelloResult{BinaryVersion: "older-build", WireVersion: daemon.WireVersion}, skewBinary},
		{"silent build", daemon.HelloResult{WireVersion: daemon.WireVersion}, skewUnknown},
		{"skewed wire", daemon.HelloResult{BinaryVersion: "router-build", WireVersion: daemon.WireVersion + 1}, skewWire},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sup.classifyWorker("session", tc.hello); got != tc.want {
				t.Errorf("classifyWorker(%+v) = %v, want %v", tc.hello, got, tc.want)
			}
		})
	}
}

// TestCreateRecordsWorkerVersionAndSurfacesItOnTheRoster drives the version
// policy through REAL spawned worker processes, end to end: the router's own
// Create path performs the gofer/hello handshake, records the worker's versions
// on its handle, classifies them, and surfaces the owning binary on the roster —
// the assertion the mixed-version upgrade demo makes through the EXISTING
// gofer/roster RPC.
//
// All three rows must stay fully routable: only a WIRE mismatch refuses new
// work, and a real worker always speaks the router's own wire version.
func TestCreateRecordsWorkerVersionAndSurfacesItOnTheRoster(t *testing.T) {
	const routerVersion = "router-under-test"
	tests := []struct {
		name          string
		workerVersion string
		wantSkew      skewClass
	}{
		{
			name:          "same build is not skew",
			workerVersion: routerVersion,
			wantSkew:      skewNone,
		},
		{
			name:          "older worker binary is recorded as binary skew",
			workerVersion: "older-worker-build",
			wantSkew:      skewBinary,
		},
		{
			// A worker built before the version wiring reports nothing. It must
			// still be routable — refusing would brick every pre-slice worker.
			name:          "worker that reports no version is unknown, not refused",
			workerVersion: "",
			wantSkew:      skewUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortRuntimeDir(t)
			root := t.TempDir()
			sup, err := New(Config{
				Root:         root,
				Version:      routerVersion,
				NewWorkerCmd: fauxWorkerSeamOpts(root, fauxWorkerOptions{Version: tc.workerVersion}),
			})
			if err != nil {
				t.Fatalf("router.New: %v", err)
			}
			t.Cleanup(func() {
				killWorkers(sup)
				_ = sup.Close()
			})

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			info, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: t.TempDir()})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			h, ok := sup.get(info.ID)
			if !ok {
				t.Fatalf("no live handle for created session %s", info.ID)
			}
			if h.binaryVersion != tc.workerVersion {
				t.Errorf("handle binaryVersion = %q, want %q (from gofer/hello)", h.binaryVersion, tc.workerVersion)
			}
			if h.wireVersion != daemon.WireVersion {
				t.Errorf("handle wireVersion = %d, want %d", h.wireVersion, daemon.WireVersion)
			}
			if h.skew != tc.wantSkew {
				t.Errorf("handle skew = %v, want %v", h.skew, tc.wantSkew)
			}
			if info.BinaryVersion != tc.workerVersion {
				t.Errorf("Create SessionInfo BinaryVersion = %q, want %q", info.BinaryVersion, tc.workerVersion)
			}

			// The roster carries the OWNING WORKER's binary version, stamped by
			// the router from the handle — a `session/list` can therefore show
			// mixed binary versions while an upgrade drains old workers.
			rows, err := sup.Roster(ctx)
			if err != nil {
				t.Fatalf("Roster: %v", err)
			}
			var found bool
			for _, r := range rows {
				if r.ID != info.ID {
					continue
				}
				found = true
				if r.BinaryVersion != tc.workerVersion {
					t.Errorf("roster row BinaryVersion = %q, want %q", r.BinaryVersion, tc.workerVersion)
				}
			}
			if !found {
				t.Fatalf("created session %s missing from the roster", info.ID)
			}

			// None of these classes refuses new work.
			if err := sup.Send(ctx, info.ID, "a turn"); err != nil {
				t.Errorf("Send on a %v worker: err = %v, want nil (only wire skew refuses)", tc.wantSkew, err)
			}
		})
	}
}
