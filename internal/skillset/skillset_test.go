package skillset_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/skillset"
)

// writeSkill creates <dir>/<name>/SKILL.md with the given frontmatter +
// body.
func writeSkill(t *testing.T, dir, name, description, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

// TestLoadProjectBeatsGlobal is the fix for the confirmed precedence bug: a
// project skill (<cwd>/.gofer/skills) must override a global skill
// (<root>/skills) of the same name — the same outcome
// internal/usercmd.Load's project-scope-overwrites-user-scope map gives
// project commands, reached here via [config.Skills.Directories]'s ordering
// feeding the SDK's first-directory-wins skill.Load.
func TestLoadProjectBeatsGlobal(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	writeSkill(t, filepath.Join(root, "skills"), "review", "the GLOBAL review skill", "global body")
	writeSkill(t, filepath.Join(cwd, ".gofer", "skills"), "review", "the PROJECT review skill", "project body")

	set, diags := skillset.Load(config.Skills{}, root, cwd)

	idx := set.Index()
	if len(idx) != 1 {
		t.Fatalf("Index() len = %d, want 1", len(idx))
	}
	if idx[0].Description != "the PROJECT review skill" {
		t.Fatalf("Index()[0].Description = %q, want the project skill's description (project must beat global)", idx[0].Description)
	}

	body, err := set.Body("review")
	if err != nil {
		t.Fatalf("Body(review) = %v", err)
	}
	if body != "project body" {
		t.Fatalf("Body(review) = %q, want %q (project must beat global)", body, "project body")
	}

	// The SDK's Load still reports the shadowed global definition as a
	// Diagnostic (see its package doc) — that must survive this wiring, not
	// be swallowed by it.
	if len(diags) == 0 {
		t.Fatal("diags = empty, want the shadowed global definition reported")
	}
}

// TestNewTool_ExcludesDisabled proves config.Skills.Disabled — a gofer
// concept the SDK's skill.Set knows nothing of — is applied at the
// invocation surface: a disabled skill neither appears in the tool's
// projected index nor answers Run.
func TestNewTool_ExcludesDisabled(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "keep", "kept skill", "keep body")
	writeSkill(t, dir, "drop", "dropped skill", "drop body")

	cfg := config.Skills{Dirs: []string{dir}, Disabled: []string{"drop"}}
	set, diags := skillset.Load(cfg, "", "")
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none", diags)
	}

	tl, ok := skillset.NewTool(set, cfg)
	if !ok {
		t.Fatal("NewTool ok = false, want true (one skill remains after disabling the other)")
	}
	if desc := tl.Description(); strings.Contains(desc, "drop") {
		t.Fatalf("Description() = %q, must not mention the disabled skill", desc)
	} else if !strings.Contains(desc, "keep") {
		t.Fatalf("Description() = %q, want it to mention the enabled skill", desc)
	}

	input, _ := json.Marshal(map[string]string{"name": "drop"})
	res, err := tl.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run(drop) error = %v", err)
	}
	if !res.IsError {
		t.Fatal("Run(drop) IsError = false, want true — a disabled skill must refuse to run")
	}
}

// TestNewTool_AllDisabledSkipsRegistration proves the caller-visible signal
// for "nothing to offer": ok=false when every discovered skill is disabled,
// so sessionGuard's caller knows not to register a permanently-empty tool.
func TestNewTool_AllDisabledSkipsRegistration(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "only", "the only skill", "body")

	cfg := config.Skills{Dirs: []string{dir}, Disabled: []string{"only"}}
	set, _ := skillset.Load(cfg, "", "")
	if _, ok := skillset.NewTool(set, cfg); ok {
		t.Fatal("NewTool ok = true, want false — every skill is disabled")
	}
}

// TestLoad_DescriptionTruncation proves config.Skills.DescriptionBytes
// reaches the SDK's DescriptionBudget.
func TestLoad_DescriptionTruncation(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a very long description word ", 20)
	writeSkill(t, dir, "verbose", long, "body")

	budget := 40
	cfg := config.Skills{Dirs: []string{dir}, DescriptionBytes: &budget}
	set, diags := skillset.Load(cfg, "", "")
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none", diags)
	}
	idx := set.Index()
	if len(idx) != 1 {
		t.Fatalf("Index() len = %d, want 1", len(idx))
	}
	if !idx[0].Truncated {
		t.Fatal("Truncated = false, want true — description exceeds the configured budget")
	}
	if len(idx[0].Description) > budget {
		t.Fatalf("Description len = %d, want <= %d (the configured budget)", len(idx[0].Description), budget)
	}
}

// TestLoad_OversizedSkillSkipped proves config.Skills.MaxFileBytes reaches
// the SDK's MaxBodyBytes, and that a diagnostic is observable rather than
// swallowed — [Summarize] is the seam sessionGuard uses to surface it.
func TestLoad_OversizedSkillSkipped(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "huge", "a skill with a huge body", strings.Repeat("x", 1000))

	maxBytes := 10
	cfg := config.Skills{Dirs: []string{dir}, MaxFileBytes: &maxBytes}
	set, diags := skillset.Load(cfg, "", "")
	if set.Len() != 0 {
		t.Fatalf("Len() = %d, want 0 — the oversized skill must be SKIPPED, not truncated", set.Len())
	}
	if len(diags) == 0 {
		t.Fatal("diags = empty, want one Diagnostic for the skipped oversized skill")
	}
	if msg := skillset.Summarize(diags); msg == "" {
		t.Fatal("Summarize(diags) = \"\", want a non-empty operator-facing note")
	}
}

// TestLoad_MissingDirectoryNotFatal proves an unconfigured/nonexistent
// skills location is the normal case, not a failure — matching
// internal/usercmd.Load's "missing directory is not a Warning" contract.
func TestLoad_MissingDirectoryNotFatal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	cwd := filepath.Join(t.TempDir(), "also-does-not-exist")

	set, diags := skillset.Load(config.Skills{}, root, cwd)
	if set == nil {
		t.Fatal("Load returned a nil Set for missing directories")
	}
	if set.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", set.Len())
	}
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none — a missing directory is not a failure", diags)
	}
}

// TestBody_LoadsOnlyOnInvocation proves progressive disclosure end to end
// through this package: the Set built by Load does not cache a skill's
// body, so a change to the file on disk AFTER Load is visible the next time
// the tool actually invokes it.
func TestBody_LoadsOnlyOnInvocation(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "live", "a skill that changes after load", "original body")

	cfg := config.Skills{Dirs: []string{dir}}
	set, diags := skillset.Load(cfg, "", "")
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none", diags)
	}

	// Mutate the file on disk after Load — if the body were cached at Load
	// time (reintroducing the field the SDK's Meta deliberately omits),
	// Run would still return the original text.
	writeSkill(t, dir, "live", "a skill that changes after load", "updated body")

	tl, ok := skillset.NewTool(set, cfg)
	if !ok {
		t.Fatal("NewTool ok = false, want true")
	}
	input, _ := json.Marshal(map[string]string{"name": "live"})
	res, err := tl.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run(live) error = %v", err)
	}
	if res.IsError {
		t.Fatalf("Run(live) IsError = true, content = %q", res.Content)
	}
	if res.Content != "updated body" {
		t.Fatalf("Run(live) content = %q, want %q — body must be read fresh, not cached from Load", res.Content, "updated body")
	}
	if !res.FullResult {
		t.Fatal("Run(live) FullResult = false, want true — a skill body must not be re-truncated by spill excerpting")
	}
}

// TestNewTool_UnknownSkill proves an unknown name is a model-correctable
// tool.Result, not a Go error, matching the SDK's own skill tool contract.
func TestNewTool_UnknownSkill(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "known", "a known skill", "body")
	cfg := config.Skills{Dirs: []string{dir}}
	set, _ := skillset.Load(cfg, "", "")

	tl, ok := skillset.NewTool(set, cfg)
	if !ok {
		t.Fatal("NewTool ok = false, want true")
	}
	input, _ := json.Marshal(map[string]string{"name": "nonexistent"})
	res, err := tl.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run(nonexistent) error = %v", err)
	}
	if !res.IsError {
		t.Fatal("Run(nonexistent) IsError = false, want true")
	}
}

// TestSummarize covers the empty/single/multi-diagnostic shapes.
func TestSummarize(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "a", "a", strings.Repeat("x", 1000))
	writeSkill(t, dir, "b", "b", strings.Repeat("x", 1000))

	if got := skillset.Summarize(nil); got != "" {
		t.Fatalf("Summarize(nil) = %q, want \"\"", got)
	}

	maxBytes := 10
	cfg := config.Skills{Dirs: []string{dir}, MaxFileBytes: &maxBytes}
	_, diags := skillset.Load(cfg, "", "")
	if len(diags) != 2 {
		t.Fatalf("diags len = %d, want 2", len(diags))
	}
	msg := skillset.Summarize(diags)
	if !strings.HasPrefix(msg, "skills: skipped ") {
		t.Fatalf("Summarize(diags) = %q, want it to start with %q", msg, "skills: skipped ")
	}
	if !strings.Contains(msg, "+1 more") {
		t.Fatalf("Summarize(diags) = %q, want it to mention the second diagnostic", msg)
	}
}
