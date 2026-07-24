package main

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// TestSubagentLinkNewSessionParams pins `gofer run --parent/--agent`'s wiring
// into the shared session/new request constructor: a plain run must send no
// `_meta` at all (the request `gofer run` has always sent), and a linked run
// must put the parent under gofer/parent and the agent under gofer/agent — not
// swapped. The constructor's own shape is pinned at its definition in
// internal/daemon; this covers THIS call site's arguments.
func TestSubagentLinkNewSessionParams(t *testing.T) {
	tests := []struct {
		name     string
		sub      subagentLink
		wantMeta map[string]string // nil ⇒ the _meta key must be absent entirely
	}{
		{"plain run sends no _meta", subagentLink{}, nil},
		{
			"a full link sends both keys",
			subagentLink{parentID: "parent-id", agent: "go-developer"},
			map[string]string{"gofer/parent": "parent-id", "gofer/agent": "go-developer"},
		},
		{"--agent alone still sends _meta", subagentLink{agent: "go-developer"}, map[string]string{"gofer/agent": "go-developer"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.sub.newSessionParams("/proj"))
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
			if got.Cwd != "/proj" {
				t.Errorf("request %s lost cwd", raw)
			}
			if !reflect.DeepEqual(got.Meta, tc.wantMeta) {
				t.Errorf("request %s _meta = %v, want %v", raw, got.Meta, tc.wantMeta)
			}
		})
	}
}

// TestRunRejectsSubagentFlagsWithoutDaemon pins the refusal `gofer run` gives
// when --parent/--agent are used with no daemon reachable: a subagent link is
// resolved, depth-capped and persisted by a supervisor, and the in-process
// fallback drives a bare runner with none, so silently creating an UNLINKED root
// session would be the one outcome the operator cannot detect. --local forces
// that path deterministically, with no dependence on whether a daemon happens to
// be running on this machine.
func TestRunRejectsSubagentFlagsWithoutDaemon(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"--parent", []string{"--local", "--parent", "some-session", "hi"}},
		{"--agent", []string{"--local", "--agent", "go-developer", "hi"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runRun(t.Context(), tc.args, nil, io.Discard, io.Discard)
			var ue *usageError
			if !errors.As(err, &ue) {
				t.Fatalf("runRun = %v, want a usage error", err)
			}
			if !strings.Contains(ue.msg, "gofer daemon") {
				t.Errorf("usage error %q does not name the remedy", ue.msg)
			}
		})
	}
}
