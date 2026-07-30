package supervisor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// promptprovenance_test.go covers [supervisor.RecordPrompt]/[supervisor.PromptText] —
// the durable trail cmd/gofer's run/resume/exec leave beside a session's
// journal so its actually-composed system prompt is greppable on disk (see
// internal/prompt's package doc).

// TestRecordPrompt_RoundTrip asserts a recorded {files, sha256, bytes} and
// composed text read back exactly through both [supervisor.ReadSidecar] and
// [supervisor.PromptText].
func TestRecordPrompt_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := "sess-1"
	journalPath := filepath.Join(dir, id+".jsonl")
	prov := supervisor.PromptProvenance{
		Files:  []string{"builtin:system.md", "AGENTS.md"},
		SHA256: "deadbeef",
		Bytes:  42,
	}
	const text = "You are gofer.\n\nRepo instructions here."

	if err := supervisor.RecordPrompt(id, journalPath, prov, text); err != nil {
		t.Fatalf("RecordPrompt: %v", err)
	}

	info := supervisor.ReadSidecar(dir, id)
	if got := info.PromptFiles; len(got) != 2 || got[0] != prov.Files[0] || got[1] != prov.Files[1] {
		t.Errorf("PromptFiles = %v, want %v", got, prov.Files)
	}
	if info.PromptSHA256 != prov.SHA256 {
		t.Errorf("PromptSHA256 = %q, want %q", info.PromptSHA256, prov.SHA256)
	}
	if info.PromptBytes != prov.Bytes {
		t.Errorf("PromptBytes = %d, want %d", info.PromptBytes, prov.Bytes)
	}

	got, err := supervisor.PromptText(dir, id)
	if err != nil {
		t.Fatalf("PromptText: %v", err)
	}
	if got != text {
		t.Errorf("PromptText = %q, want %q", got, text)
	}
}

// TestRecordPrompt_PreservesSubagentLink asserts RecordPrompt's
// read-modify-write does not clobber a parent/agent link already recorded in
// the sidecar — the two writers (a spawn's link write, a create's prompt
// write) touch the same file and must compose, not race each other out.
func TestRecordPrompt_PreservesSubagentLink(t *testing.T) {
	dir := t.TempDir()
	id := "child-1"
	journalPath := filepath.Join(dir, id+".jsonl")

	// Simulate the subagent-link write RecordPrompt must not clobber: write a
	// sidecar carrying it directly, mirroring what Supervisor.Spawn would have
	// already persisted for this child before its prompt is composed.
	raw := `{"parentId":"parent-1","agent":"go-developer","depth":1}`
	if err := os.WriteFile(filepath.Join(dir, id+".meta.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}

	if err := supervisor.RecordPrompt(id, journalPath, supervisor.PromptProvenance{
		Files: []string{"builtin:system.md"}, SHA256: "abc", Bytes: 3,
	}, "abc"); err != nil {
		t.Fatalf("RecordPrompt: %v", err)
	}

	info := supervisor.ReadSidecar(dir, id)
	if info.ParentID != "parent-1" || info.Agent != "go-developer" || info.Depth != 1 {
		t.Errorf("subagent link = %+v, want preserved {parent-1, go-developer, 1}", info)
	}
	if info.PromptSHA256 != "abc" {
		t.Errorf("PromptSHA256 = %q, want %q (prompt write should still land)", info.PromptSHA256, "abc")
	}
}

// TestPromptText_MissingIsError asserts PromptText surfaces a plain not-exist
// error for a session with no recorded prompt, rather than degrading to an
// empty string — unlike the sidecar's own zero-value-on-missing contract, a
// caller reading prompt text back wants to know the difference between "no
// prompt was ever recorded" and "the prompt was recorded as empty text".
func TestPromptText_MissingIsError(t *testing.T) {
	dir := t.TempDir()
	if _, err := supervisor.PromptText(dir, "nope"); err == nil {
		t.Fatal("PromptText: want an error for a missing file, got nil")
	}
}
