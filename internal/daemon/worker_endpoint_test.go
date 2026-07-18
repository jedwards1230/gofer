package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadWorkerEndpointRoundTrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	const uuid = "sess-roundtrip"
	want := WorkerEndpoint{
		Addr:          "unix:///run/user/1000/gofer-1000/workers/" + uuid + ".sock",
		PID:           4242,
		BinaryVersion: "v1.2.3",
		WireVersion:   1,
		StartedAt:     time.Now().UTC().Truncate(time.Second),
	}

	if err := WriteWorkerEndpoint(uuid, want); err != nil {
		t.Fatalf("WriteWorkerEndpoint: %v", err)
	}

	// The file is written mode 0600 (per-user runtime state — pid/versions —
	// no other user has business reading it), like the daemon endpoint file.
	path, err := WorkerEndpointPath(uuid)
	if err != nil {
		t.Fatalf("WorkerEndpointPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat endpoint file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("endpoint file mode = %o, want 600", perm)
	}

	got, err := ReadWorkerEndpoint(uuid)
	if err != nil {
		t.Fatalf("ReadWorkerEndpoint: %v", err)
	}
	if got.Addr != want.Addr || got.PID != want.PID ||
		got.BinaryVersion != want.BinaryVersion || got.WireVersion != want.WireVersion {
		t.Errorf("endpoint = %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
}

func TestReadWorkerEndpointMissing(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	_, err := ReadWorkerEndpoint("nope")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadWorkerEndpoint of missing uuid: got %v, want errors.Is os.ErrNotExist", err)
	}
}

func TestRemoveWorkerEndpointIdempotent(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	const uuid = "sess-remove"
	if err := WriteWorkerEndpoint(uuid, WorkerEndpoint{Addr: "unix://x.sock", PID: 1}); err != nil {
		t.Fatalf("WriteWorkerEndpoint: %v", err)
	}
	if err := RemoveWorkerEndpoint(uuid); err != nil {
		t.Fatalf("first RemoveWorkerEndpoint: %v", err)
	}
	// Already gone → still nil.
	if err := RemoveWorkerEndpoint(uuid); err != nil {
		t.Fatalf("second RemoveWorkerEndpoint: %v", err)
	}
}

func TestListWorkerEndpoints(t *testing.T) {
	t.Run("missing dir returns empty and no error", func(t *testing.T) {
		// A fresh temp root where the workers subdir was never created.
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

		got, err := ListWorkerEndpoints()
		if err != nil {
			t.Fatalf("ListWorkerEndpoints: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d entries, want 0", len(got))
		}
	})

	t.Run("failure matrix: skips corrupt, returns valid sorted", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

		dir, err := WorkersDir()
		if err != nil {
			t.Fatalf("WorkersDir: %v", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir workers: %v", err)
		}

		// (a) two valid endpoint files — written out of sort order to prove
		//     ListWorkerEndpoints sorts.
		valid := map[string]WorkerEndpoint{
			"sess-bbb": {Addr: "unix://bbb.sock", PID: 200, BinaryVersion: "v2", WireVersion: 1},
			"sess-aaa": {Addr: "unix://aaa.sock", PID: 100, BinaryVersion: "v1", WireVersion: 1},
		}
		for uuid, ep := range valid {
			if err := WriteWorkerEndpoint(uuid, ep); err != nil {
				t.Fatalf("WriteWorkerEndpoint %s: %v", uuid, err)
			}
		}

		// The corrupt/ignored files, written directly.
		writeRaw := func(name, body string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		writeRaw("sess-corrupt.json", "{ this is not json") // (b) corrupt JSON
		writeRaw("sess-empty.json", "")                     // (c) empty/partial
		writeRaw("notes.txt", "not an endpoint")            // (d) non-.json
		writeRaw(".worker-1234.json.tmp", `{"addr":"x"}`)   // (e) in-progress temp
		// (f) an unreadable (mode 0000) endpoint file is also skipped by the
		//     same ReadWorkerEndpoint-error branch, but is intentionally NOT
		//     exercised here: chmod 0000 is a no-op for root, which CI often
		//     runs as, so the case would pass or fail depending on the runner's
		//     uid rather than on the code. The corrupt/empty cases above cover
		//     the same skip-on-read-error path deterministically.

		got, err := ListWorkerEndpoints()
		if err != nil {
			t.Fatalf("ListWorkerEndpoints: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2: %+v", len(got), got)
		}
		if got[0].UUID != "sess-aaa" || got[1].UUID != "sess-bbb" {
			t.Errorf("entries not sorted by UUID: %q, %q", got[0].UUID, got[1].UUID)
		}
		if got[0].Endpoint.Addr != "unix://aaa.sock" || got[1].Endpoint.Addr != "unix://bbb.sock" {
			t.Errorf("decoded wrong endpoints: %+v", got)
		}
	})
}
