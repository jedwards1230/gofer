package skillset_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	sdkskill "github.com/jedwards1230/agent-sdk-go/skill"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/skillset"
)

// TestIsShadowedRecognizesARealDuplicate drives the SDK loader for real rather
// than hand-building a Diagnostic, because the whole classification rests on
// the loader's own message: a test that constructed the error itself would
// keep passing after the SDK reworded it, which is exactly the drift this test
// is here to notice.
func TestIsShadowedRecognizesARealDuplicate(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	writeShadowSkill(t, filepath.Join(cwd, ".gofer", "skills", "clash"), "clash", "the winner")
	loser := filepath.Join(root, "skills", "clash")
	writeShadowSkill(t, loser, "clash", "the loser")

	_, diags := skillset.Load(config.Skills{}, root, cwd)
	if len(diags) != 1 {
		t.Fatalf("expected exactly the duplicate diagnostic, got %+v", diags)
	}
	if !skillset.IsShadowed(diags[0]) {
		t.Errorf("IsShadowed(%v) = false; the SDK duplicate message may have changed", diags[0])
	}
	if diags[0].Path != filepath.Join(loser, "SKILL.md") {
		t.Errorf("diagnostic path = %q, want the LOSING file", diags[0].Path)
	}
}

// TestIsShadowedRejectsOtherDiagnostics pins the other half: an ordinary
// load failure must NOT be labelled shadowed. Mislabelling one would tell an
// operator their skill lost a name clash when in fact it is malformed — a
// wrong diagnosis is worse than the unlabelled truth.
func TestIsShadowedRejectsOtherDiagnostics(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills", "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No frontmatter at all: a candidate the loader refuses for a reason that
	// has nothing to do with precedence.
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("just a body\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, diags := skillset.Load(config.Skills{}, root, t.TempDir())
	if len(diags) == 0 {
		t.Fatal("expected a diagnostic for a frontmatter-less SKILL.md")
	}
	for _, d := range diags {
		if skillset.IsShadowed(d) {
			t.Errorf("IsShadowed(%v) = true for a non-duplicate diagnostic", d)
		}
	}
}

// TestIsShadowedOnDegenerateDiagnostics covers the two values that never come
// out of a real Load but are trivially constructible by a future caller: a
// nil-error diagnostic must not panic, and an unrelated error must not be
// mistaken for a duplicate.
func TestIsShadowedOnDegenerateDiagnostics(t *testing.T) {
	if skillset.IsShadowed(sdkskill.Diagnostic{}) {
		t.Error("a nil-error diagnostic must not be shadowed")
	}
	if skillset.IsShadowed(sdkskill.Diagnostic{Path: "/x/SKILL.md", Err: errors.New("boom")}) {
		t.Error("an unrelated error must not be shadowed")
	}
}

func writeShadowSkill(t testing.TB, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nthe body\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s/SKILL.md: %v", dir, err)
	}
}
