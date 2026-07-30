package lspdiag

import (
	"context"
	"testing"
	"time"

	sdklsp "github.com/jedwards1230/agent-sdk-go/lsp"
)

// registryWithFakeServer returns a Registry that "knows about" language but
// whose command is never on PATH — deterministic regardless of what happens
// to be installed on the machine running the test, unlike relying on
// (or the absence of) a real gopls.
func registryWithFakeServer(language string) *sdklsp.Registry {
	reg := sdklsp.NewRegistry()
	reg.Register(sdklsp.Server{Language: language, Command: "lspdiag-test-nonexistent-binary"})
	return reg
}

func TestDiagnose_UnsupportedExtension_NoServerTouched(t *testing.T) {
	mgr := NewManager()
	mgr.registry = registryWithFakeServer("go") // would fail loudly if ever consulted
	got := mgr.Diagnose(context.Background(), t.TempDir(), "notes.txt", "hello", Options{Enabled: true})
	if got != nil {
		t.Errorf("Diagnose() = %v, want nil for an unsupported extension", got)
	}
}

func TestDiagnose_ServerNotOnPath_DegradesToNil(t *testing.T) {
	mgr := NewManager()
	mgr.registry = registryWithFakeServer("go")
	got := mgr.Diagnose(context.Background(), t.TempDir(), "main.go", "package main", Options{Enabled: true, Timeout: time.Second})
	if got != nil {
		t.Errorf("Diagnose() = %v, want nil when the registered server is not on PATH", got)
	}
	if err := mgr.Close(); err != nil {
		t.Errorf("Close() = %v, want nil (no server was ever started)", err)
	}
}

func TestDiagnose_NoServerRegisteredForLanguage(t *testing.T) {
	mgr := NewManager()
	mgr.registry = sdklsp.NewRegistry() // empty: nothing registered for any language
	got := mgr.Diagnose(context.Background(), t.TempDir(), "main.go", "package main", Options{Enabled: true})
	if got != nil {
		t.Errorf("Diagnose() = %v, want nil when no server is registered at all", got)
	}
}

func TestDiagnose_AfterClose_ReturnsNilImmediately(t *testing.T) {
	mgr := NewManager()
	mgr.registry = registryWithFakeServer("go")
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	got := mgr.Diagnose(context.Background(), t.TempDir(), "main.go", "package main", Options{Enabled: true})
	if got != nil {
		t.Errorf("Diagnose() after Close = %v, want nil", got)
	}
	// Close is idempotent.
	if err := mgr.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
}

func TestPublish_DeliversToArmedWaiter(t *testing.T) {
	mgr := NewManager()
	uri := "file:///tmp/f.go"
	ch := mgr.arm(uri)

	batch := sdklsp.Batch{URI: uri, Items: []sdklsp.Diagnostic{{Message: "boom"}}}
	mgr.Publish(context.Background(), "tag", batch)

	select {
	case got := <-ch:
		if len(got.Items) != 1 || got.Items[0].Message != "boom" {
			t.Errorf("delivered batch = %+v, want the published one", got)
		}
	default:
		t.Fatal("expected the armed waiter to receive the published batch")
	}
}

func TestPublish_UnarmedURI_NeverBlocks(t *testing.T) {
	mgr := NewManager()
	// No waiter registered for this URI at all — Publish must be a no-op, not
	// a block or a panic.
	mgr.Publish(context.Background(), "tag", sdklsp.Batch{URI: "file:///tmp/unwatched.go"})
}

func TestDisarm_RemovesOnlyItsOwnWaiter(t *testing.T) {
	mgr := NewManager()
	uri := "file:///tmp/f.go"
	ch1 := mgr.arm(uri)
	ch2 := mgr.arm(uri)

	mgr.disarm(uri, ch1)

	mgr.Publish(context.Background(), "tag", sdklsp.Batch{URI: uri})
	select {
	case <-ch1:
		t.Error("disarmed channel should not have received the batch")
	default:
	}
	select {
	case <-ch2:
	default:
		t.Error("the still-armed channel should have received the batch")
	}
}

func TestCapDiagnostics(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	got := capDiagnostics(items, 3)
	want := []string{"a", "b", "c", "… +2 more"}
	if len(got) != len(want) {
		t.Fatalf("capDiagnostics() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("capDiagnostics()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if got := capDiagnostics(items, 10); len(got) != len(items) {
		t.Errorf("capDiagnostics() with max >= len should return items unchanged, got %v", got)
	}
}

func TestLanguageForPath(t *testing.T) {
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"main.go", "go", true},
		{"a/b/C.GO", "go", true}, // extension match is case-insensitive
		{"index.tsx", "typescript", true},
		{"app.py", "python", true},
		{"README.md", "", false},
		{"noext", "", false},
	}
	for _, c := range cases {
		got, ok := languageForPath(c.path)
		if got != c.want || ok != c.ok {
			t.Errorf("languageForPath(%q) = (%q, %v), want (%q, %v)", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestFileURI(t *testing.T) {
	if got, want := fileURI("/tmp/x/main.go"), "file:///tmp/x/main.go"; got != want {
		t.Errorf("fileURI() = %q, want %q", got, want)
	}
}
