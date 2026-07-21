package daemonbridge

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/tui"
)

// TestStatusFromWireMapping locks the daemon's roster status STRING → TUI
// enum mapping for all three states plus an unrecognized value. It is an
// internal (package daemonbridge) test so it can call statusFromWire
// directly: the live M2 daemon never actually emits "finished"
// (supervisor.SessionStatus's doc: reserved, never emitted in M2), so a
// round trip through a real daemon (see bridge_test.go's
// TestRosterReflectsCreatedSession, which covers "needs-input" end to end)
// can never exercise that branch or the unrecognized-value fallback.
func TestStatusFromWireMapping(t *testing.T) {
	cases := []struct {
		wire string
		want tui.SessionStatus
	}{
		{"working", tui.StatusWorking},
		{"needs-input", tui.StatusNeedsInput},
		{"finished", tui.StatusFinished},
		{"unknown", tui.StatusNeedsInput},
		{"", tui.StatusNeedsInput},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			if got := statusFromWire(tc.wire); got != tc.want {
				t.Errorf("statusFromWire(%q) = %v, want %v", tc.wire, got, tc.want)
			}
		})
	}
}

// TestToTUISessionInfoMapsPending locks the wire DTO's "pending" field
// (contract #2 of the M3 approvals-relay work) into tui.SessionInfo.Pending —
// the count [tui.Overview]'s statusGlyph renders as the roster's ✋N badge.
// An unset (zero-value/omitted) field maps to 0, matching M2's always-0
// behavior for a daemon that hasn't yet started sending it.
func TestToTUISessionInfoMapsPending(t *testing.T) {
	cases := []struct {
		name string
		dto  sessionInfoDTO
		want int
	}{
		{"zero value", sessionInfoDTO{}, 0},
		{"positive count", sessionInfoDTO{Pending: 2}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toTUISessionInfo(tc.dto).Pending; got != tc.want {
				t.Errorf("toTUISessionInfo(%+v).Pending = %d, want %d", tc.dto, got, tc.want)
			}
		})
	}
}

// TestToTUISessionInfoMapsSubagentLink locks the wire row's subagent fields into
// tui.SessionInfo — the link a tree render indents children by. An unset row (a
// root session, or any daemon predating subagents) must map to the zero values,
// which is exactly "a root session".
func TestToTUISessionInfoMapsSubagentLink(t *testing.T) {
	cases := []struct {
		name string
		dto  sessionInfoDTO
		want tui.SessionInfo
	}{
		{"root session", sessionInfoDTO{}, tui.SessionInfo{}},
		{
			"child session",
			sessionInfoDTO{ParentID: "parent-id", Agent: "go-developer", Depth: 2},
			tui.SessionInfo{ParentID: "parent-id", Agent: "go-developer", Depth: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toTUISessionInfo(tc.dto)
			if got.ParentID != tc.want.ParentID || got.Agent != tc.want.Agent || got.Depth != tc.want.Depth {
				t.Errorf("toTUISessionInfo(%+v) link = {%q, %q, %d}, want {%q, %q, %d}",
					tc.dto, got.ParentID, got.Agent, got.Depth,
					tc.want.ParentID, tc.want.Agent, tc.want.Depth)
			}
		})
	}
}

// TestCreateForwardsSubagentOptions pins THIS bridge's argument wiring into the
// shared request constructor — that opts.ParentID lands on gofer/parent and
// opts.Agent on gofer/agent, not swapped. The constructor's own shape (raw keys,
// no `_meta` for a plain create) is pinned once, at its definition, in
// internal/daemon.
func TestCreateForwardsSubagentOptions(t *testing.T) {
	opts := tui.CreateOptions{Cwd: "/proj", Model: "faux", ParentID: "parent-id", Agent: "go-developer"}
	raw, err := json.Marshal(daemon.NewSessionRequestFor(opts.Cwd, opts.Model, opts.ParentID, opts.Agent))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Cwd  string            `json:"cwd"`
		Meta map[string]string `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	want := map[string]string{"gofer/parent": "parent-id", "gofer/agent": "go-developer"}
	if got.Cwd != opts.Cwd || !reflect.DeepEqual(got.Meta, want) {
		t.Errorf("request %s = {cwd %q, _meta %v}, want {%q, %v}", raw, got.Cwd, got.Meta, opts.Cwd, want)
	}
}
