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

// TestFleetFooterRendersTotal pins that the fleet-total footer renders the
// daemon-aggregated cost/usage and the live-session count beneath the table when
// the daemon supports it (worker mode). This is the client half of the fleet
// cost/usage aggregation feature — the total the router computes off its roster
// cache reaching an operator's screen.
func TestFleetFooterRendersTotal(t *testing.T) {
	rows := []psRow{
		{ID: "s1", Status: "working", Live: true, Cost: psCost{USD: 0.10}},
		{ID: "s2", Status: "working", Live: true, Cost: psCost{USD: 0.25}},
	}
	fleet := psFleet{
		Supported: true,
		Cost:      psCost{USD: 0.35},
		Usage:     psUsage{InputTokens: 200, OutputTokens: 100},
	}

	var out strings.Builder
	writeFleetFooter(&out, fleet, rows)
	got := out.String()

	for _, want := range []string{"Fleet:", "$0.3500", "300 tokens", "2 live"} {
		if !strings.Contains(got, want) {
			t.Errorf("fleet footer missing %q:\n%s", want, got)
		}
	}
}

// TestFleetFooterOmittedWhenUnsupported pins that an in-process or older daemon —
// which reports no fleet total (supported=false) — produces NO footer, so
// `gofer ps` looks exactly as it did before this feature.
func TestFleetFooterOmittedWhenUnsupported(t *testing.T) {
	var out strings.Builder
	writeFleetFooter(&out, psFleet{Supported: false}, []psRow{{ID: "s1", Live: true}})
	if got := out.String(); got != "" {
		t.Errorf("fleet footer rendered for an unsupported daemon: %q, want empty", got)
	}
}

// TestFleetFooterCountsOnlyLive pins that the footer's session count is the LIVE
// rows (the aggregation is live-only), so a `gofer ps --all` view mixing offline
// rows still reports the total across the live fleet.
func TestFleetFooterCountsOnlyLive(t *testing.T) {
	rows := []psRow{
		{ID: "s1", Live: true, Cost: psCost{USD: 0.10}},
		{ID: "s2", Live: false},
		{ID: "s3", Live: true, Cost: psCost{USD: 0.20}},
	}
	var out strings.Builder
	writeFleetFooter(&out, psFleet{Supported: true, Cost: psCost{USD: 0.30}}, rows)
	if got := out.String(); !strings.Contains(got, "2 live") {
		t.Errorf("fleet footer live count wrong (want 2 live):\n%s", got)
	}
}

// TestFleetDecodesWire pins the gofer/fleet decode against internal/daemon's
// fleetUsageDTO — a tag drift would silently blank the footer rather than fail
// to build.
func TestFleetDecodesWire(t *testing.T) {
	var fleet psFleet
	body := `{"supported":true,"cost":{"usd":0.5},"usage":{"input_tokens":10,"output_tokens":20}}`
	if err := json.Unmarshal([]byte(body), &fleet); err != nil {
		t.Fatalf("decoding a fleet reply: %v", err)
	}
	if !fleet.Supported || fleet.Cost.USD != 0.5 || fleet.Usage.InputTokens != 10 || fleet.Usage.OutputTokens != 20 {
		t.Errorf("psFleet decoded = %+v, want supported/0.5/10/20 — check the json tags match the daemon's", fleet)
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
