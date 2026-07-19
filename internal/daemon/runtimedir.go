package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// maxUnixSocketPath is the conservative usable-length budget for a unix-domain
// socket path across the platforms gofer runs on. macOS's sockaddr_un.sun_path
// is 104 bytes including the NUL terminator (103 usable); Linux allows 108. We
// guard against the smaller (macOS) limit everywhere so a socket path that
// binds fine on Linux CI can never silently fail bind() on a macOS host.
const maxUnixSocketPath = 103

// workerSocketHashLen is how many hex chars of the session-id hash name a
// worker socket (see [WorkerSocketPath]). 16 hex = 64 bits — collision-safe for
// the tens of concurrent sessions this targets — while keeping the basename
// short enough to stay within maxUnixSocketPath under a long macOS $TMPDIR.
const workerSocketHashLen = 16

// WorkersDir returns the per-user directory that holds the M6 per-session
// worker runtime files — endpoint (`<uuid>.json`), socket (`<hash>.sock`, see
// [WorkerSocketPath] for why the socket alone is not `<uuid>.sock`), and
// single-writer lock (`<uuid>.lock`) — namespaced by uid as
// `<runtime>/gofer-<uid>/workers`.
//
// The runtime root follows the same XDG-vs-TempDir scheme the daemon's own
// socket path uses:
//
//   - `$XDG_RUNTIME_DIR` when set and non-empty (the Linux per-user runtime
//     dir, typically `/run/user/<uid>`, mode 0700, cleaned on logout), else
//   - `os.TempDir()` — the macOS/BSD fallback. `XDG_RUNTIME_DIR` is a
//     Linux-ism systemd provides and macOS does not; there `os.TempDir()`
//     (i.e. `$TMPDIR`) resolves to the per-user, mode-0700 confstr
//     DARWIN_USER_TEMP_DIR, giving the same single-user isolation.
//
// The runtime root is kept deliberately short. Worker sockets live in this
// directory, and a unix-domain socket path has a hard ~104-byte limit on
// macOS (108 on Linux) — rooting under a deep home-relative path would risk
// overflowing it. Even under this short root the budget is tight: a full
// 36-char UUIDv7 basename plus a long macOS `$TMPDIR` fallback
// (`/var/folders/xx/<28>/T/`) exceeds macOS's 103-byte sun_path — which is why
// [WorkerSocketPath] names the socket with a short id HASH, not the full uuid,
// and length-guards the result (see [maxUnixSocketPath]). The `<uuid>.json` and
// `<uuid>.lock` files are ordinary PATH_MAX-bounded filenames and carry the
// full uuid.
//
// WorkersDir has no side effects — it does not create the directory. Callers
// that need it created (WriteWorkerEndpoint, LockWorker) MkdirAll it at mode
// 0700 themselves. Resolution reads the environment (`XDG_RUNTIME_DIR`), so a
// test can redirect the whole scheme by setting that variable.
func WorkersDir() (string, error) {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = os.TempDir()
	}
	uid := os.Getuid()
	return filepath.Join(runtime, "gofer-"+strconv.Itoa(uid), "workers"), nil
}

// WorkerEndpointPath returns the path of a worker's endpoint file,
// `<WorkersDir>/<uuid>.json` — the atomically written advertisement
// (`{addr, pid, binaryVersion, wireVersion, startedAt}`) that
// WriteWorkerEndpoint/ReadWorkerEndpoint/RemoveWorkerEndpoint operate on.
func WorkerEndpointPath(uuid string) (string, error) {
	dir, err := WorkersDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, uuid+".json"), nil
}

// WorkerSocketPath returns the path of a worker's unix-domain socket, the
// address the worker listens on and advertises via its endpoint file's Addr
// (the router reads Addr from the file and never re-derives this).
//
// The basename is a short hash of the session id — `<hash>.sock`, NOT
// `<uuid>.sock` — deliberately, to keep the path within the unix-domain socket
// length budget (see [maxUnixSocketPath]). A full 36-char UUIDv7 basename plus
// a long macOS `$TMPDIR` fallback (`/var/folders/xx/<28>/T/`) overflows macOS's
// 103-byte sun_path — so the socket is the ONE per-worker file that cannot
// carry the full id. The `<uuid>.json` endpoint and `<uuid>.lock` (ordinary
// filenames, PATH_MAX-bounded) still carry it in full.
//
// A hash rather than a uuid prefix on purpose: a UUIDv7's leading 48 bits are a
// millisecond timestamp, so two sessions created in the same millisecond can
// share a raw prefix — hashing the whole id is collision-safe regardless of the
// id scheme. As defense-in-depth (a pathologically long `$XDG_RUNTIME_DIR` or
// `$TMPDIR` can still blow the budget) the composed path is length-checked and
// an over-budget path is a returned error, not a socket that fails to bind.
func WorkerSocketPath(uuid string) (string, error) {
	dir, err := WorkersDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(uuid))
	name := hex.EncodeToString(sum[:])[:workerSocketHashLen] + ".sock"
	path := filepath.Join(dir, name)
	if len(path) > maxUnixSocketPath {
		return "", fmt.Errorf("daemon: worker socket path %q is %d bytes, exceeds the %d-byte unix-domain socket limit", path, len(path), maxUnixSocketPath)
	}
	return path, nil
}

// WorkerLockPath returns the path of a worker's single-writer lock file,
// `<WorkersDir>/<uuid>.lock` — the gofer-side advisory flock LockWorker
// acquires to enforce one live worker per session id (design §4).
func WorkerLockPath(uuid string) (string, error) {
	dir, err := WorkersDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, uuid+".lock"), nil
}
