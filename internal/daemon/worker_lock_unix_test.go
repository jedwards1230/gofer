//go:build unix

package daemon

import (
	"errors"
	"testing"
)

func TestLockWorker(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	const uuid = "sess-lock"

	// First acquisition succeeds.
	release, err := LockWorker(uuid)
	if err != nil {
		t.Fatalf("first LockWorker: %v", err)
	}

	// A second acquisition on the same uuid contends. flock is per open file
	// description, so a distinct os.OpenFile of the same path — even in this
	// same process — does not share the first's lock and gets EWOULDBLOCK.
	if _, err := LockWorker(uuid); !errors.Is(err, ErrWorkerLocked) {
		t.Fatalf("second LockWorker while held: got %v, want ErrWorkerLocked", err)
	}

	// Release the first, then a fresh acquisition succeeds again.
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	release2, err := LockWorker(uuid)
	if err != nil {
		t.Fatalf("LockWorker after release: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("release2: %v", err)
	}
}
