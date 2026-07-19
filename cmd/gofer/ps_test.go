package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPSTableRendersBinaryVersion is the `gofer ps` half of M6 §11's
// "session/list shows mixed binaryVersions" criterion. Under process isolation a
// daemon upgrade drains rather than migrates: sessions already running finish on
// the build they started with while new ones come up on the new build. The
// version reached the wire before slice 3b but no client rendered it, so that
// drain was invisible to an operator. It is now a BINARY column.
func TestPSTableRendersBinaryVersion(t *testing.T) {
	rows := []psRow{
		{ID: "0192a1b2-0000-7000-8000-000000000001", Status: "working", Model: "fable-5", Project: "gofer", Live: true, BinaryVersion: "0.3.0"},
		{ID: "0192a1b2-0000-7000-8000-000000000002", Status: "working", Model: "fable-5", Project: "gofer", Live: true, BinaryVersion: "0.2.9"},
	}

	var out strings.Builder
	writePSTable(&out, rows, false)
	got := out.String()

	if !strings.Contains(got, "BINARY") {
		t.Errorf("ps table has no BINARY header column:\n%s", got)
	}
	// Both versions must be present and distinguishable — that IS the criterion.
	for _, want := range []string{"0.3.0", "0.2.9"} {
		if !strings.Contains(got, want) {
			t.Errorf("ps table does not render binary version %q:\n%s", want, got)
		}
	}
}

// TestPSTableEmptyBinaryVersion pins the placeholder for a row with no version:
// an offline session (no process to have a build) and any pre-M6 daemon (never
// sends the field) both produce "". Rendering that as a blank cell reads as a
// broken table, so it renders as an explicit "-".
func TestPSTableEmptyBinaryVersion(t *testing.T) {
	var out strings.Builder
	writePSTable(&out, []psRow{{ID: "sess-1", Status: "finished", Project: "gofer"}}, true)
	got := out.String()

	if !strings.Contains(got, "-") {
		t.Errorf("ps table does not render the missing-version placeholder %q:\n%s", "-", got)
	}
}

// TestPSRowDecodesBinaryVersion pins the wire tag against internal/daemon's
// sessionInfoDTO. `gofer ps` decodes the daemon's roster JSON independently
// (this IS the public wire contract — any ACP client decodes it the same way),
// so a tag typo here would silently blank the column rather than fail to build.
func TestPSRowDecodesBinaryVersion(t *testing.T) {
	var row psRow
	if err := json.Unmarshal([]byte(`{"id":"s","binaryVersion":"0.4.1"}`), &row); err != nil {
		t.Fatalf("decoding a roster row: %v", err)
	}
	if row.BinaryVersion != "0.4.1" {
		t.Errorf("psRow.BinaryVersion = %q, want %q — check the json tag matches the daemon's", row.BinaryVersion, "0.4.1")
	}
}
