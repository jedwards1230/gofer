package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// endpointFileName is the endpoint file's name within its root directory.
const endpointFileName = "daemon.json"

// Endpoint is the daemon's own listen address and auth token, advertised on
// disk (see [EndpointPath]) so a client on the same host can discover a
// running daemon instead of requiring an operator to pass --daemon/--token
// by hand (see cmd/gofer's daemonFlags.resolve).
//
// SECURITY: Endpoint carries the bearer token in cleartext (Token) — the
// same sensitivity class as auth.json. Never log an Endpoint value, its
// Token field, or the raw file contents anywhere, not even at debug level.
// [WriteEndpoint] enforces this at rest by writing the file at mode 0600.
type Endpoint struct {
	// Addr is the daemon's listen address, exactly as configured
	// ([Config.ListenAddr]) — a bare host:port; callers prefix ws:// as
	// needed (see wsURL).
	Addr string `json:"addr"`
	// Token is the daemon's configured bearer token, or "" when auth is
	// disabled (a loopback-only daemon started with no --token). See the
	// security note on [Endpoint] — never log this field.
	Token string `json:"token,omitempty"`
	// PID is the daemon process's own pid, used to detect (and self-heal
	// past) a stale file a crash left behind rather than a clean shutdown
	// removing it.
	PID int `json:"pid"`
	// StartedAt is when the daemon wrote this file. Informational only; no
	// code currently branches on it.
	StartedAt time.Time `json:"started_at"`
}

// EndpointPath returns the path [WriteEndpoint]/[ReadEndpoint]/[RemoveEndpoint]
// use for a given session-store root, resolving an empty root through
// [supervisor.ResolveRoot] — the same default (~/.gofer) the session store
// itself uses — so the endpoint file always lands alongside the store it
// advertises. Resolution is intentionally shared (not re-derived) so a client
// and its daemon can never disagree about where a default-root daemon's
// endpoint file lives.
func EndpointPath(root string) (string, error) {
	resolved, err := supervisor.ResolveRoot(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, endpointFileName), nil
}

// WriteEndpoint persists ep at [EndpointPath](root), atomically: it writes a
// sibling temp file at mode 0600 then renames it over the target, so a crash
// mid-write never leaves a truncated or partially written file (mirroring
// the SDK auth store's auth.json write — see agent-sdk-go/auth.Store.save).
// The containing directory is created (mode 0700) if it does not exist yet.
//
// The file's mode is 0600, not 0644 — it carries a bearer token in
// cleartext (see [Endpoint]'s security note). WriteEndpoint itself never
// logs ep or any part of it.
func WriteEndpoint(root string, ep Endpoint) error {
	path, err := EndpointPath(root)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemon: create %s: %w", dir, err)
	}

	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon: encode endpoint: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".daemon-*.json.tmp")
	if err != nil {
		return fmt.Errorf("daemon: create temp endpoint file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("daemon: chmod temp endpoint file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("daemon: write temp endpoint file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("daemon: sync temp endpoint file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("daemon: close temp endpoint file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("daemon: replace %s: %w", path, err)
	}
	return nil
}

// ReadEndpoint reads and decodes the endpoint file at [EndpointPath](root).
// A missing file is reported as an error satisfying errors.Is(err,
// os.ErrNotExist) — the standard, wrapped way — so a caller distinguishes
// "no daemon has ever advertised one here" from a genuine read/parse
// failure without string-matching. ReadEndpoint never logs the decoded
// value.
func ReadEndpoint(root string) (Endpoint, error) {
	path, err := EndpointPath(root)
	if err != nil {
		return Endpoint{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		// %w preserves errors.Is(err, os.ErrNotExist) through the wrap (the
		// underlying *PathError already satisfies it), so a caller can tell
		// "no daemon has advertised one here" apart from a genuine I/O
		// failure without string-matching this message.
		return Endpoint{}, fmt.Errorf("daemon: read %s: %w", path, err)
	}
	var ep Endpoint
	if err := json.Unmarshal(b, &ep); err != nil {
		return Endpoint{}, fmt.Errorf("daemon: parse %s: %w", path, err)
	}
	return ep, nil
}

// RemoveEndpoint removes the endpoint file at [EndpointPath](root). It is
// not an error for the file to already be gone.
func RemoveEndpoint(root string) error {
	path, err := EndpointPath(root)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("daemon: remove %s: %w", path, err)
	}
	return nil
}
