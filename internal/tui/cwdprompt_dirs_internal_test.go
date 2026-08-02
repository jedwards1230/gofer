package tui

// Internal because [directoriesOf] is the seam the ABSOLUTE-candidates contract
// lives on, and asserting it through the rendered prompt would only prove the
// display path — which contracts paths back to "~" for rendering anyway
// (homepath.go), and would therefore look identical whether the underlying
// candidate was absolute or not.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirectoriesOfAlwaysReturnsAbsoluteTildeFreeCandidates pins the contract
// [directoriesOf] states: every candidate it emits is absolute and carries no
// unexpanded "~", whatever shape the base arrives in.
//
// The "~/…" base is the case that motivated it. [CommandEnv.Cwd] is free-form
// enough to hold one, and seeding from it verbatim emitted candidates like
// "~/projects/sub" — not absolute, and tilde-bearing — from a function whose
// doc promises the opposite. [resolveChosenDir] expands whatever the user picks,
// so nothing malformed ever reached the wire; the point of asserting it here is
// that the guarantee stops depending on that one downstream caller remembering
// to.
func TestDirectoriesOfAlwaysReturnsAbsoluteTildeFreeCandidates(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory to expand against")
	}

	for _, tc := range []struct {
		name  string
		base  string
		paths []string
	}{
		{"tilde base", "~/projects", []string{"sub/a.go", "sub/deep/b.go"}},
		{"bare tilde base", "~", []string{"sub/a.go"}},
		{"absolute base", "/tmp/projects", []string{"sub/a.go"}},
		{"relative base", "projects", []string{"sub/a.go"}},
		{"untrimmed tilde base", "  ~/projects  ", []string{"sub/a.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := directoriesOf(tc.paths, tc.base, 16)
			if len(got) == 0 {
				t.Fatalf("directoriesOf(%q) returned nothing; want at least the base", tc.base)
			}
			for _, dir := range got {
				if !filepath.IsAbs(dir) {
					t.Errorf("candidate %q is not absolute (base %q)", dir, tc.base)
				}
				if strings.Contains(dir, "~") {
					t.Errorf("candidate %q still carries an unexpanded %q (base %q)", dir, "~", tc.base)
				}
			}
		})
	}
}

// TestDirectoriesOfDedupesAcrossBaseSpellings is the other half of normalizing
// base up front: the dedup key is the emitted path, so a base that was not
// normalized deduped in the WRONG namespace. "~/projects" and its expansion are
// one directory, and a candidate list that offered the user both would be
// offering the same directory twice under two names.
func TestDirectoriesOfDedupesAcrossBaseSpellings(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory to expand against")
	}
	paths := []string{"sub/a.go", "sub/b.go", "other/c.go"}

	tilde := directoriesOf(paths, "~/projects", 16)
	expanded := directoriesOf(paths, filepath.Join(home, "projects"), 16)

	if len(tilde) != len(expanded) {
		t.Fatalf("tilde base produced %d candidates, expanded base %d — want identical sets\n tilde=%v\n expanded=%v",
			len(tilde), len(expanded), tilde, expanded)
	}
	for i := range tilde {
		if tilde[i] != expanded[i] {
			t.Errorf("candidate %d: tilde base gave %q, expanded base gave %q", i, tilde[i], expanded[i])
		}
	}
}
