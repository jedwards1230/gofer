package daemon

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSessionInfoDTORoundTrip pins the roster row's wire shape end to end:
// supervisor snapshot → DTO → JSON → DTO. The JSON hop is the point — the keys
// are the contract every gofer client decodes (internal/wirestream's mirror of
// this type, and through it internal/daemonbridge and internal/router), so a
// renamed or dropped tag surfaces here rather than as a silently empty field in
// a client.
func TestSessionInfoDTORoundTrip(t *testing.T) {
	created := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	in := supervisor.SessionInfo{
		ID:       "0192a1b2-0000-7000-8000-00000000000a",
		Title:    "own the subagent primitive",
		Status:   supervisor.StatusWorking,
		Model:    "claude-sonnet-4-5",
		Created:  created,
		Updated:  created.Add(time.Minute),
		Project:  "gofer",
		Live:     true,
		Cwd:      "/proj",
		ParentID: "0192a1b2-0000-7000-8000-000000000001",
		Agent:    "go-developer",
		Depth:    2,
	}

	raw, err := json.Marshal(toSessionInfoDTO(in))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got sessionInfoDTO
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}

	if got.ParentID != in.ParentID {
		t.Errorf("parentId = %q, want %q (raw %s)", got.ParentID, in.ParentID, raw)
	}
	if got.Agent != in.Agent {
		t.Errorf("agent = %q, want %q (raw %s)", got.Agent, in.Agent, raw)
	}
	if got.Depth != in.Depth {
		t.Errorf("depth = %d, want %d (raw %s)", got.Depth, in.Depth, raw)
	}
	// The pre-existing fields must survive the additions untouched.
	if got.ID != in.ID || got.Title != in.Title || got.Status != "working" || got.Cwd != in.Cwd {
		t.Errorf("round trip lost a pre-existing field: %+v", got)
	}

	// The wire KEYS, not just the Go field names: a client decodes these
	// literals.
	var keyed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keyed); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, key := range []string{"parentId", "agent", "depth"} {
		if _, ok := keyed[key]; !ok {
			t.Errorf("wire row %s is missing key %q", raw, key)
		}
	}
}

// TestNewSessionRequestForMetaOnlyWhenLinked is the canonical pin for the
// session/new REQUEST shape. It is asserted once, here, because every producer
// in the repo — internal/daemonbridge, the M6 router's router→worker call, and
// cmd/gofer's `gofer run` — goes through this one constructor: the keys can only
// live in struct tags, so a second declaration of the shape anywhere would be a
// second, independently-typo-able copy, and a typo in the REQUEST direction
// fails SILENTLY (a plain root session, no error on either end).
//
// Two properties matter: a plain create emits no `_meta` at all (byte-identical
// to what gofer sent before subagents existed), and a linked create emits
// exactly the gofer-namespaced keys handleSessionNew decodes.
func TestNewSessionRequestForMetaOnlyWhenLinked(t *testing.T) {
	cases := []struct {
		name     string
		parentID string
		agent    string
		wantMeta map[string]string // nil ⇒ the _meta key must be absent entirely
	}{
		{"plain create sends no _meta", "", "", nil},
		{
			"a full link sends both keys", "parent-id", "go-developer",
			map[string]string{"gofer/parent": "parent-id", "gofer/agent": "go-developer"},
		},
		{"a parent alone still sends _meta", "parent-id", "", map[string]string{"gofer/parent": "parent-id"}},
		{"an agent alone still sends _meta", "", "go-developer", map[string]string{"gofer/agent": "go-developer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(NewSessionRequestFor("/proj", "faux", tc.parentID, tc.agent))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got struct {
				Cwd   string            `json:"cwd"`
				Model string            `json:"model"`
				Meta  map[string]string `json:"_meta"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if got.Cwd != "/proj" || got.Model != "faux" {
				t.Errorf("request %s lost an ACP field", raw)
			}
			if !reflect.DeepEqual(got.Meta, tc.wantMeta) {
				t.Errorf("request %s _meta = %v, want %v", raw, got.Meta, tc.wantMeta)
			}

			// And the daemon's own decode must read back what the producer wrote —
			// the two halves of the contract, pinned against each other.
			var decoded NewSessionRequest
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			gotParent, gotAgent, _ := decoded.Meta.SubagentLink()
			if gotParent != tc.parentID || gotAgent != tc.agent {
				t.Errorf("decoded link = {%q, %q}, want {%q, %q}", gotParent, gotAgent, tc.parentID, tc.agent)
			}
		})
	}
}

// TestNewSessionMetaAccessorsAreNilSafe covers the "daemon predating the field"
// path: a response with no `_meta` at all must fall back to the requested model
// and report an empty link rather than panicking on a nil Meta.
func TestNewSessionMetaAccessorsAreNilSafe(t *testing.T) {
	var resp NewSessionResponse
	if err := json.Unmarshal([]byte(`{"sessionId":"s1"}`), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Meta != nil {
		t.Fatalf("Meta = %+v, want nil for a response with no _meta", resp.Meta)
	}
	if got := resp.Meta.ModelOr("requested-model"); got != "requested-model" {
		t.Errorf("ModelOr = %q, want the requested fallback", got)
	}
	if p, a, d := resp.Meta.SubagentLink(); p != "" || a != "" || d != 0 {
		t.Errorf("SubagentLink = {%q, %q, %d}, want all zero", p, a, d)
	}
	// A present meta wins over the requested fallback.
	resp.Meta = &NewSessionMeta{Model: "assigned-model"}
	if got := resp.Meta.ModelOr("requested-model"); got != "assigned-model" {
		t.Errorf("ModelOr = %q, want the assigned model", got)
	}
}

// TestSessionInfoDTOOmitsSubagentFieldsForRootSession pins the omitempty
// polarity: an ordinary root session — every session gofer created before
// subagents existed — must serialize byte-for-byte as it did before, so the
// three new keys are absent rather than present-and-empty.
func TestSessionInfoDTOOmitsSubagentFieldsForRootSession(t *testing.T) {
	raw, err := json.Marshal(toSessionInfoDTO(supervisor.SessionInfo{ID: "root", Live: true}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keyed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keyed); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, key := range []string{"parentId", "agent", "depth"} {
		if _, ok := keyed[key]; ok {
			t.Errorf("root-session row %s carries %q, want it omitted", raw, key)
		}
	}
}
