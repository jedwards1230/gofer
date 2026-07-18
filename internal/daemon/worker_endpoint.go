package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// WorkerEndpoint is a single-session worker's on-disk advertisement, written
// atomically at startup to `<WorkersDir>/<uuid>.json` (see
// [WorkerEndpointPath]) and removed on clean exit. It mirrors the daemon's own
// endpoint-file precedent ([Endpoint]) one tier deeper: where the daemon
// advertises itself to same-host clients, each worker advertises itself to the
// router so a freshly (re)started router can discover and adopt live workers
// by scanning the workers dir (design §4).
//
// The session uuid is intentionally NOT a field here — it is the file's
// basename (`<uuid>.json`), so it is passed separately to
// [WriteWorkerEndpoint]/[ReadWorkerEndpoint]/[RemoveWorkerEndpoint] and
// returned alongside the endpoint by [ListWorkerEndpoints].
//
// Unlike [Endpoint], a WorkerEndpoint carries no bearer token — its fields are
// host-local process metadata (pid/version). The file is still written mode
// 0600 for consistency with the daemon endpoint and because a worker's pid and
// versions are per-user runtime state that other users have no business
// reading.
type WorkerEndpoint struct {
	// Addr is the worker's listen address — `unix://<WorkersDir>/<uuid>.sock`
	// (see [WorkerSocketPath]) — that the router dials to adopt the worker.
	Addr string `json:"addr"`
	// PID is the worker process's own pid, used by the router's adoption scan
	// for a signal-0 liveness probe (`pidAlive`) before it bothers to dial.
	PID int `json:"pid"`
	// BinaryVersion is the worker's build version, the cheap pre-dial version
	// hint the router reads from the file to make an adopt/spawn/skew-route
	// decision (design §6); the authoritative value comes later from the
	// in-protocol `gofer/hello` handshake.
	BinaryVersion string `json:"binaryVersion"`
	// WireVersion is the router↔worker wire-protocol version the worker
	// speaks. A plain int on this branch (the WireVersion constant that will
	// source it is introduced by a separate M6 PR).
	WireVersion int `json:"wireVersion"`
	// StartedAt is when the worker wrote this file. Informational; the router
	// does not branch on it today.
	StartedAt time.Time `json:"startedAt"`
}

// WriteWorkerEndpoint persists ep at [WorkerEndpointPath](uuid), atomically: it
// writes a sibling temp file at mode 0600 then renames it over the target, so a
// crash mid-write never leaves a truncated or partially written file that the
// router's scan could misread. The containing workers dir is created (mode
// 0700) if it does not exist yet. Mirrors [WriteEndpoint].
func WriteWorkerEndpoint(uuid string, ep WorkerEndpoint) error {
	path, err := WorkerEndpointPath(uuid)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemon: create %s: %w", dir, err)
	}

	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon: encode worker endpoint: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".worker-*.json.tmp")
	if err != nil {
		return fmt.Errorf("daemon: create temp worker endpoint file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("daemon: chmod temp worker endpoint file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("daemon: write temp worker endpoint file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("daemon: sync temp worker endpoint file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("daemon: close temp worker endpoint file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("daemon: replace %s: %w", path, err)
	}
	return nil
}

// ReadWorkerEndpoint reads and decodes the endpoint file at
// [WorkerEndpointPath](uuid). A missing file is reported as an error
// satisfying errors.Is(err, os.ErrNotExist) — the standard, wrapped way — so a
// caller distinguishes "no worker has advertised one for this uuid" from a
// genuine read/parse failure without string-matching. Mirrors [ReadEndpoint].
func ReadWorkerEndpoint(uuid string) (WorkerEndpoint, error) {
	path, err := WorkerEndpointPath(uuid)
	if err != nil {
		return WorkerEndpoint{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		// %w preserves errors.Is(err, os.ErrNotExist) through the wrap (the
		// underlying *PathError already satisfies it), so a caller can tell a
		// never-advertised uuid apart from a genuine I/O failure without
		// string-matching this message.
		return WorkerEndpoint{}, fmt.Errorf("daemon: read %s: %w", path, err)
	}
	var ep WorkerEndpoint
	if err := json.Unmarshal(b, &ep); err != nil {
		return WorkerEndpoint{}, fmt.Errorf("daemon: parse %s: %w", path, err)
	}
	return ep, nil
}

// RemoveWorkerEndpoint removes the endpoint file at [WorkerEndpointPath](uuid).
// It is not an error for the file to already be gone (a clean-exit removal
// racing the router's stale-file unlink, or a double removal). Mirrors
// [RemoveEndpoint].
func RemoveWorkerEndpoint(uuid string) error {
	path, err := WorkerEndpointPath(uuid)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("daemon: remove %s: %w", path, err)
	}
	return nil
}

// WorkerEndpointEntry pairs a worker's session uuid (its endpoint file's
// basename) with the decoded endpoint, as returned by [ListWorkerEndpoints].
type WorkerEndpointEntry struct {
	// UUID is the session id, derived from the `<uuid>.json` filename.
	UUID string
	// Endpoint is the decoded advertisement.
	Endpoint WorkerEndpoint
}

// ListWorkerEndpoints scans [WorkersDir] and returns one entry per valid
// `<uuid>.json` endpoint file, sorted by UUID for deterministic iteration —
// the router's adoption-scan input (design §4).
//
// Discovery is deliberately fault-tolerant: a single stale, corrupt, partial,
// or unreadable file must never break the whole scan (§4's failure matrix). So
// entries that fail to read or decode are SKIPPED, not surfaced as an error;
// non-`.json` entries and in-progress `*.tmp` temp files (which share the
// `.tmp` suffix, not `.json`) are ignored. A missing workers dir — no worker
// has ever started — is not an error either: it yields an empty slice.
func ListWorkerEndpoints() ([]WorkerEndpointEntry, error) {
	dir, err := WorkersDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("daemon: scan %s: %w", dir, err)
	}

	var out []WorkerEndpointEntry
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		// Only `<uuid>.json`. WriteWorkerEndpoint's temp files are
		// `.worker-*.json.tmp` — the `.tmp` suffix excludes them here.
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		uuid := strings.TrimSuffix(name, ".json")
		ep, err := ReadWorkerEndpoint(uuid)
		if err != nil {
			// Stale/corrupt/partial/unreadable — skip, never fail the scan.
			continue
		}
		out = append(out, WorkerEndpointEntry{UUID: uuid, Endpoint: ep})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out, nil
}
