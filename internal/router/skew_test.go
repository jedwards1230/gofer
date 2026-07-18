package router

import (
	"errors"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemon"
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
