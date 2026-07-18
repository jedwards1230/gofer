package daemon_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestWriteEndpointMode0600 asserts the on-disk endpoint file is written at
// mode 0600 — it carries a bearer token in cleartext, the same sensitivity
// class as auth.json (see [daemon.Endpoint]'s security note).
func TestWriteEndpointMode0600(t *testing.T) {
	root := t.TempDir()
	ep := daemon.Endpoint{Addr: "127.0.0.1:7333", Token: "s3cr3t", PID: 1234, StartedAt: time.Now()}
	if err := daemon.WriteEndpoint(root, ep); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	path, err := daemon.EndpointPath(root)
	if err != nil {
		t.Fatalf("EndpointPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("endpoint file mode = %o, want 0600", perm)
	}
}

// TestEndpointRoundTrip covers WriteEndpoint/ReadEndpoint round-tripping
// every field, including a zero-value Token (the no-auth loopback case).
func TestEndpointRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := daemon.Endpoint{
		Addr:      "192.168.8.179:7333",
		Token:     "the-token",
		PID:       4321,
		StartedAt: time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
		Version:   "v1.2.3",
	}
	if err := daemon.WriteEndpoint(root, want); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	got, err := daemon.ReadEndpoint(root)
	if err != nil {
		t.Fatalf("ReadEndpoint: %v", err)
	}
	// Compare StartedAt with time.Equal (a struct == on time.Time is brittle
	// across a JSON round trip — monotonic readings and wall/ext
	// representations can differ even for the same instant) and every other
	// field directly.
	if got.Addr != want.Addr || got.Token != want.Token || got.PID != want.PID || !got.StartedAt.Equal(want.StartedAt) || got.Version != want.Version {
		t.Errorf("ReadEndpoint = %+v, want %+v", got, want)
	}
}

// TestEndpointRoundTripNoToken covers the no-auth (loopback, no --token)
// case: Token round-trips as "" rather than some other zero-value artifact
// of the omitempty tag.
func TestEndpointRoundTripNoToken(t *testing.T) {
	root := t.TempDir()
	want := daemon.Endpoint{Addr: "127.0.0.1:7333", PID: 99, StartedAt: time.Now().Truncate(time.Second)}
	if err := daemon.WriteEndpoint(root, want); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	got, err := daemon.ReadEndpoint(root)
	if err != nil {
		t.Fatalf("ReadEndpoint: %v", err)
	}
	if got.Token != "" {
		t.Errorf("Token = %q, want \"\"", got.Token)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	// An old daemon that predates the Version field never set it; omitempty
	// means it round-trips as "" (unknown), not some other artifact — the
	// signal the CLI's skew check treats as "skip, don't warn".
	if got.Version != "" {
		t.Errorf("Version = %q, want \"\"", got.Version)
	}
}

// TestEndpointWireVersionRoundTrip covers the additive WireVersion field
// (design §6): a set value round-trips through WriteEndpoint/ReadEndpoint.
func TestEndpointWireVersionRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := daemon.Endpoint{Addr: "127.0.0.1:7333", PID: 7, StartedAt: time.Now(), WireVersion: 3}
	if err := daemon.WriteEndpoint(root, want); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	got, err := daemon.ReadEndpoint(root)
	if err != nil {
		t.Fatalf("ReadEndpoint: %v", err)
	}
	if got.WireVersion != 3 {
		t.Errorf("WireVersion = %d, want 3", got.WireVersion)
	}
}

// TestEndpointWireVersionOmitEmpty asserts a zero WireVersion round-trips as 0
// AND is omitted from the marshaled JSON (omitempty) — an older writer that
// never set it reads back as 0 (unknown), never a phantom version.
func TestEndpointWireVersionOmitEmpty(t *testing.T) {
	root := t.TempDir()
	ep := daemon.Endpoint{Addr: "127.0.0.1:7333", PID: 7, StartedAt: time.Now()}
	if err := daemon.WriteEndpoint(root, ep); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	got, err := daemon.ReadEndpoint(root)
	if err != nil {
		t.Fatalf("ReadEndpoint: %v", err)
	}
	if got.WireVersion != 0 {
		t.Errorf("WireVersion = %d, want 0", got.WireVersion)
	}

	// Marshal the zero-WireVersion Endpoint ourselves and assert the key is
	// absent — the omitempty guarantee the endpoint-file compat window relies on.
	b, err := json.Marshal(ep)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if bytes.Contains(b, []byte("wireVersion")) {
		t.Errorf("marshaled zero-WireVersion Endpoint contains \"wireVersion\" key: %s", b)
	}
}

// TestReadEndpointMissingIsErrNotExist asserts a never-written endpoint file
// is reported as an error satisfying errors.Is(err, os.ErrNotExist) — the
// distinguishable "no daemon has ever advertised one here" signal
// [daemonFlags.resolve]-equivalent callers branch on.
func TestReadEndpointMissingIsErrNotExist(t *testing.T) {
	root := t.TempDir()
	_, err := daemon.ReadEndpoint(root)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadEndpoint on a missing file: err = %v, want errors.Is(err, os.ErrNotExist)", err)
	}
}

// TestRemoveEndpointLifecycle covers RemoveEndpoint on both an existing file
// (it disappears) and an already-absent one (no error).
func TestRemoveEndpointLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:7333", PID: 1}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	if err := daemon.RemoveEndpoint(root); err != nil {
		t.Fatalf("RemoveEndpoint (existing): %v", err)
	}
	if _, err := daemon.ReadEndpoint(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadEndpoint after RemoveEndpoint: err = %v, want os.ErrNotExist", err)
	}

	// Removing again (already absent) must not error.
	if err := daemon.RemoveEndpoint(root); err != nil {
		t.Fatalf("RemoveEndpoint (already absent): %v", err)
	}
}

// TestWriteEndpointCreatesRootDir asserts WriteEndpoint creates its root
// directory (mode 0700) rather than requiring the caller to pre-create it —
// the same convenience auth.json's store offers.
func TestWriteEndpointCreatesRootDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-yet-created")
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:7333", PID: 1}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("Stat(root): %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", root)
	}
}

// TestEndpointPathDefaultRoot asserts an empty root resolves through
// [supervisor.ResolveRoot]'s default (~/.gofer) rather than some
// independently hardcoded path — the whole point of sharing the resolver is
// that a client and its daemon can never disagree about where the endpoint
// file for a default-root daemon lives.
func TestEndpointPathDefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := daemon.EndpointPath("")
	if err != nil {
		t.Fatalf("EndpointPath(\"\"): %v", err)
	}
	want := filepath.Join(home, ".gofer", "daemon.json")
	if path != want {
		t.Errorf("EndpointPath(\"\") = %q, want %q", path, want)
	}
}
