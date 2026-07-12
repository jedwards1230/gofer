// Package testkit is the golden-file harness for gofer's TUI components. It
// never spins a real terminal: components expose a plain View(width, height)
// method that testkit calls directly and compares byte-for-byte against a
// checked-in testdata/*.golden file.
//
// Determinism comes from the caller, not this package: build the component
// under test with [theme.Test], which forces termenv.Ascii so lipgloss never
// emits color codes, and always render at the same fixed size (see [Width],
// [Height]) so wrapping and truncation are stable across machines and CI.
//
// Golden files are captured, not hand-written:
//
//	go test ./internal/tui/... -run TestGolden -update
//
// Review the diff on every -update run — a golden file is a committed
// assertion about output, not a cache.
package testkit

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update, when set via -update, (re)writes golden files from the current
// output instead of comparing against them.
var update = flag.Bool("update", false, "update golden files instead of comparing against them")

// Fixed render dimensions every golden test renders at. A component that
// only implements View(w, h) and reflows never needs to know these values
// directly; tests pass them through explicitly for readability at the call
// site.
const (
	Width  = 80
	Height = 24
)

// Renderable is anything that can render itself to a plain string at a fixed
// width and height. gofer's TUI components implement this directly, with no
// bubbletea dependency required to exercise them in a golden test.
type Renderable interface {
	View(width, height int) string
}

// Render renders v at the given size. It exists so call sites read as intent
// ("render this component at a fixed size") rather than a bare method call.
func Render(v Renderable, width, height int) string {
	return v.View(width, height)
}

// AssertGolden compares got against testdata/<name>.golden, failing the test
// with a diff on mismatch. Run the package's tests with -update to
// (re)capture the golden file from the current output.
func AssertGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", name+".golden")

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("testkit: mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("testkit: write golden %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testkit: read golden %s: %v (run with -update to create it)", path, err)
	}

	if got != string(want) {
		t.Errorf("testkit: golden mismatch for %s (run with -update to refresh, then review the diff)\n--- got ---\n%s\n--- want ---\n%s",
			name, got, string(want))
	}
}
