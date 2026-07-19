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
//
// [ColorTheme] and [AssertGoldenStyled] add a second, color-aware golden
// layer: the marker vocabulary carries state only through color (a plain
// Ascii golden renders every "●" identically regardless of whether it means
// running, done, or failed), so a state regression that only changes color
// can pass every Ascii golden. AssertGoldenStyled renders through a real
// color profile, translates the ANSI it emits to stable `<tag>...</tag>`
// markers keyed by [Theme]'s semantic styles, and diffs that against a
// checked-in testdata/*.styled.golden file — a colorless, machine-independent
// way to assert "this glyph is yellow, not green".
package testkit

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/muesli/termenv"

	"github.com/jedwards1230/gofer/internal/tui/theme"
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

// ColorTheme returns the one canonical colored test theme: [theme.Test] with
// a real color profile forced on, so lipgloss actually emits ANSI instead of
// theme.Test's default no-op styling. Every colored TUI test — the ANSI-width
// layout tests and the styled-golden state tests alike — builds its component
// through this theme, so there is exactly one "what does a colored render
// look like" definition to keep in sync with [Theme]'s tokens.
func ColorTheme() theme.Theme {
	th := theme.Test()
	th.Profile = termenv.TrueColor
	return th
}

// AssertGoldenStyled compares coloredGot — a component rendered through
// [ColorTheme] — against testdata/<name>.styled.golden, after translating its
// ANSI escapes to stable `<tag>...</tag>` markers (see [styleTags]). This is
// the color-state oracle the plain [AssertGolden] can't be: under
// theme.Test()'s Ascii profile every marker renders identically regardless of
// which state color it carries, so only a colored render can assert "this ●
// is yellow, not green". Mirrors AssertGolden's read/write/diff structure,
// including the same -update flag.
func AssertGoldenStyled(t *testing.T, name, coloredGot string) {
	t.Helper()

	got := TagANSI(t, coloredGot)
	path := filepath.Join("testdata", name+".styled.golden")

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("testkit: mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("testkit: write styled golden %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testkit: read styled golden %s: %v (run with -update to create it)", path, err)
	}

	if got != string(want) {
		t.Errorf("testkit: styled golden mismatch for %s (run with -update to refresh, then review the diff)\n--- got ---\n%s\n--- want ---\n%s",
			name, got, string(want))
	}
}

// styleTags maps each of [ColorTheme]'s known foreground styles' opening SGR
// escape sequence to a stable tag name, for [TagANSI] to translate ANSI into.
func styleTags() map[string]string {
	th := ColorTheme()
	open := func(s lipgloss.Style) string {
		r := s.Render("\x00")
		return r[:strings.IndexByte(r, 0)]
	}
	return map[string]string{
		open(th.WarnStyle()):   "yellow",
		open(th.OKStyle()):     "green",
		open(th.DangerStyle()): "red",
		open(th.MutedStyle()):  "muted",
		open(th.AccentStyle()): "accent",
		open(th.InkStyle()):    "ink",
	}
}

// sgrPattern matches one ANSI SGR (Select Graphic Rendition) escape sequence
// — lipgloss's [ColorTheme] renders emit a FLAT stream of these (an opening
// foreground-color escape, then a bare reset), never true nesting, which is
// what makes the stateful scan below correct.
var sgrPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// TagANSI translates coloredGot's ANSI escapes into stable `<tag>...</tag>`
// markers keyed by [styleTags], for a colorless, machine-independent
// comparison. [AssertGoldenStyled] uses it to build the golden; it is exported
// so a test can also assert a SINGLE style directly ("this note is yellow")
// without committing a whole frame — a golden's failure mode is a wall of diff
// that says only "something moved", which is a poor oracle for a one-line
// color contract (see internal/tui's status-severity tests).
//
// It walks the string, emitting literal text between escapes;
// each escape either closes the currently open tag (a bare reset), opens a
// known tag (closing any already-open one first), or — if it matches no known
// style — fails the test loudly, since an unrecognized escape is exactly the
// kind of silent drift this layer exists to catch.
func TagANSI(t *testing.T, coloredGot string) string {
	t.Helper()

	tags := styleTags()
	var b strings.Builder
	current := ""
	last := 0
	for _, loc := range sgrPattern.FindAllStringIndex(coloredGot, -1) {
		start, end := loc[0], loc[1]
		b.WriteString(coloredGot[last:start])
		last = end

		seq := coloredGot[start:end]
		params := seq[2 : len(seq)-1] // strip "\x1b[" and "m"
		if params == "" || params == "0" {
			if current != "" {
				b.WriteString("</" + current + ">")
				current = ""
			}
			continue
		}

		tag, ok := tags[seq]
		if !ok {
			t.Fatalf("testkit: unrecognized ANSI escape %q — add its style to styleTags or fix the render", seq)
		}
		if current != "" {
			b.WriteString("</" + current + ">")
		}
		b.WriteString("<" + tag + ">")
		current = tag
	}
	b.WriteString(coloredGot[last:])
	if current != "" {
		b.WriteString("</" + current + ">")
	}
	return b.String()
}
