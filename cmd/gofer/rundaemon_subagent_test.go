package main

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
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

// TestRouterDialBackGatedOnSubagentsOptIn is the credential-exposure gate.
//
// Under --workers the router hands every worker its dial-back coordinates, and
// the token half is the daemon's own bearer token — a credential equivalent to
// arbitrary code execution as the daemon's user. An operator who never enabled
// subagents must not pay that exposure, and must see no change to a worker at
// all: this is the one place the feature could otherwise reach a user who
// declined it.
//
// Both halves are asserted, and the token half separately: an address with no
// token would dial a token-required router unauthenticated, which is a
// confusing 401 rather than a clean "not configured".
func TestRouterDialBackGatedOnSubagentsOptIn(t *testing.T) {
	const token = "daemon-bearer-token"
	tests := []struct {
		name      string
		cfg       config.Subagents
		listen    string
		wantAddr  string
		wantToken string
	}{
		{"disabled hands over nothing", config.Subagents{}, "127.0.0.1:7333", "", ""},
		{"disabled with agents listed still hands over nothing",
			config.Subagents{Agents: []string{"go-developer"}}, "127.0.0.1:7333", "", ""},
		{"enabled hands over both", config.Subagents{Enabled: true}, "127.0.0.1:7333", "127.0.0.1:7333", token},
		// A wildcard bind is a valid thing to BIND and a meaningless thing to
		// DIAL; normalizing to loopback is correct (the worker is always on the
		// same host) and tighter than forwarding it verbatim.
		{"wildcard bind normalizes to loopback", config.Subagents{Enabled: true}, "0.0.0.0:7333", "127.0.0.1:7333", token},
		{"empty host normalizes to loopback", config.Subagents{Enabled: true}, ":7333", "127.0.0.1:7333", token},
		{"ipv6 wildcard normalizes to loopback", config.Subagents{Enabled: true}, "[::]:7333", "127.0.0.1:7333", token},
		{"an explicit host is left alone", config.Subagents{Enabled: true}, "192.168.1.50:7333", "192.168.1.50:7333", token},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := routerDialBackAddr(tc.cfg, tc.listen); got != tc.wantAddr {
				t.Errorf("routerDialBackAddr(%q) = %q, want %q", tc.listen, got, tc.wantAddr)
			}
			if got := routerDialBackToken(tc.cfg, token); got != tc.wantToken {
				t.Errorf("routerDialBackToken = %q, want %q", got, tc.wantToken)
			}
		})
	}
}
