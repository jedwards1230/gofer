//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrWorkerLocked is returned by [LockWorker] when another live worker already
// holds the session's `<uuid>.lock` — the non-blocking flock (`LOCK_NB`)
// returned EWOULDBLOCK/EAGAIN. It is the authoritative signal that a worker
// for this session id is already running (the guard against a racing
// adopt+spawn, design §4); a caller that sees it must not open the runner and
// should exit.
var ErrWorkerLocked = errors.New("daemon: worker already locked for this session")

// LockWorker takes an exclusive, non-blocking advisory lock on the worker's
// `<uuid>.lock` file (see [WorkerLockPath]) — the gofer single-writer-per-
// session guard (design §4). The SDK journal has no cross-process flock of its
// own (only the auth store's auth.lock), so gofer enforces one live worker per
// session id here.
//
// On success it returns a release func that drops the lock and closes the file;
// on contention it returns [ErrWorkerLocked]. The lock file is created if
// absent and never removed — unlinking it would race a concurrent holder
// (another process could open a fresh file at the same path and both would
// "hold" distinct inodes) — mirroring the SDK auth store's auth.lock lifetime.
func LockWorker(uuid string) (release func() error, err error) {
	path, err := WorkerLockPath(uuid)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		// LOCK_NB surfaces contention as EWOULDBLOCK (== EAGAIN on all unix
		// targets); map it to the sentinel, wrap anything else.
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrWorkerLocked
		}
		return nil, fmt.Errorf("daemon: lock %s: %w", path, err)
	}
	return func() error {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, nil
}
