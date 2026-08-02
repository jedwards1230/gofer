package tui

// homeheader_test.go pins ONE property: the identity header's cwd renders
// "~"-relative, the same as the roster's group header ([Overview.cwdLabel])
// and the status panel.
//
// It is a file of its own because the golden suite structurally cannot see
// this property, and the reason is worth stating where the next person will
// read it. [GoldenMeta]'s Cwd is the literal string "~/orchestration"
// (fixtures_test.go) — already contracted, and [displayHome] returns a path
// with no $HOME prefix unchanged. So every golden renders byte-identically
// whether or not the header contracts anything, and
// TestAppHeaderOnEveryScreen's `"claude-sonnet-5 · ~/orchestration"`
// assertion holds *vacuously* for contraction: the tilde it matches came from
// the fixture, not from the code under test.
//
// That is exactly how the header shipped spelling $HOME out in full while the
// group header two lines below it contracted the same path. A fixture that
// pre-bakes the transformation cannot test the transformation.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

func TestIdentityHeaderContractsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no resolvable home directory; displayHome degrades to the raw path by design")
	}

	abs := filepath.Join(home, "orchestration", "repos", "gofer")
	meta := OverviewMeta{App: "gofer", Version: "0.3.0", Model: "gpt-5.6-sol", Cwd: abs}

	// Wide enough that truncate() cannot be what removes the home prefix —
	// otherwise a narrow width would make this pass for the wrong reason.
	lines := identityHeaderLines(theme.Test(), meta, 4096)
	if len(lines) < 2 {
		t.Fatalf("identityHeaderLines returned %d lines, want at least 2", len(lines))
	}
	context := lines[1]

	want := filepath.Join("~", "orchestration", "repos", "gofer")
	if !strings.Contains(context, want) {
		t.Errorf("header context %q does not contain the contracted cwd %q", context, want)
	}
	if strings.Contains(context, home) {
		t.Errorf("header context %q still spells $HOME (%q) out in full; it must render \"~\"-relative "+
			"like the group header below it (Overview.cwdLabel)", context, home)
	}
}
