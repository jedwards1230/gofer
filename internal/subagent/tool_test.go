package subagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/subagent"
)

// fakeSpawner records what the tool asked for and returns a scripted answer.
type fakeSpawner struct {
	childID string
	err     error

	calls []spawnCall
}

type spawnCall struct{ parentID, agent, prompt string }

func (f *fakeSpawner) Spawn(_ context.Context, parentID, agent, prompt string) (string, error) {
	f.calls = append(f.calls, spawnCall{parentID, agent, prompt})
	return f.childID, f.err
}

// TestNewToolRegistrationGate covers both halves of the constructor's ok: the
// config opt-in, and the defensive nil-seam check. The second is unreachable
// from internal/supervisor (which always installs a seam) and kept anyway,
// because a nil seam would otherwise register a tool that panics on first call.
func TestNewToolRegistrationGate(t *testing.T) {
	spawner := &fakeSpawner{childID: "child-1"}
	cases := []struct {
		name    string
		spawner subagent.Spawner
		cfg     config.Subagents
		want    bool
	}{
		{"zero config is disabled", spawner, config.Subagents{}, false},
		{"explicitly disabled", spawner, config.Subagents{Enabled: false}, false},
		{"nil seam is never registered", nil, config.Subagents{Enabled: true}, false},
		{"enabled with a seam", spawner, config.Subagents{Enabled: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tl, ok := subagent.NewTool(c.spawner, c.cfg)
			if ok != c.want {
				t.Fatalf("NewTool ok = %v, want %v", ok, c.want)
			}
			if !ok && tl != nil {
				t.Fatalf("NewTool returned a tool alongside ok=false: %#v", tl)
			}
		})
	}
}

// TestSpecConstrainsAgentOnlyWhenConfigured pins the schema's one variable
// part: an operator who named the agents gets an enum, one who did not gets a
// free-form string. An enum of zero values would make the argument
// unsatisfiable, which is why "no agents configured" must not produce one.
func TestSpecConstrainsAgentOnlyWhenConfigured(t *testing.T) {
	tests := []struct {
		name     string
		agents   []string
		wantEnum []string
	}{
		{"unconfigured stays free-form", nil, nil},
		{"blank and duplicate entries are dropped", []string{"a", " ", "a", "b"}, []string{"a", "b"}},
		{"configured order is preserved", []string{"go-reviewer", "go-developer"}, []string{"go-reviewer", "go-developer"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tl, ok := subagent.NewTool(&fakeSpawner{}, config.Subagents{Enabled: true, Agents: tc.agents})
			if !ok {
				t.Fatal("NewTool ok = false")
			}
			got := tl.Spec().Properties["agent"].Enum
			if strings.Join(got, ",") != strings.Join(tc.wantEnum, ",") {
				t.Fatalf("agent enum = %v, want %v", got, tc.wantEnum)
			}
			if !strings.Contains(strings.Join(tl.Spec().Required, ","), "prompt") {
				t.Errorf("prompt is not required; Required = %v", tl.Spec().Required)
			}
		})
	}
}

// TestRunRefusalsAreToolResults pins [tool.Tool.Run]'s (Result, error) split
// for every refusal a model could react to. Each of these used to be a
// plausible place to return a Go error, and each would abort the parent's whole
// turn if it did.
func TestRunRefusalsAreToolResults(t *testing.T) {
	tests := []struct {
		name       string
		bind       string
		spawner    *fakeSpawner
		input      string
		wantSubstr string
	}{
		{"empty prompt", "parent-1", &fakeSpawner{childID: "c"}, `{"agent":"a"}`, "prompt must not be empty"},
		{"whitespace prompt", "parent-1", &fakeSpawner{childID: "c"}, `{"prompt":"   "}`, "prompt must not be empty"},
		{"unbound tool", "", &fakeSpawner{childID: "c"}, `{"prompt":"go"}`, "no session id bound"},
		{"seam refusal", "parent-1", &fakeSpawner{err: errors.New("depth 6 exceeds the cap of 5")}, `{"prompt":"go"}`, "exceeds the cap"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tl, ok := subagent.NewTool(tc.spawner, config.Subagents{Enabled: true})
			if !ok {
				t.Fatal("NewTool ok = false")
			}
			if tc.bind != "" {
				tl.Bind(tc.bind)
			}
			res, err := tl.Run(context.Background(), json.RawMessage(tc.input))
			if err != nil {
				t.Fatalf("Run returned a Go error, want a non-fatal result: %v", err)
			}
			if !res.IsError {
				t.Fatalf("Run result is not an error: %s", res.Content)
			}
			if !strings.Contains(res.Content, tc.wantSubstr) {
				t.Errorf("result %q does not contain %q", res.Content, tc.wantSubstr)
			}
		})
	}
}

// TestRunGoErrorsAreOnlyTheContractedTwo is the other side of that split:
// undecodable input and a cancelled ctx are the ONLY Go errors.
func TestRunGoErrorsAreOnlyTheContractedTwo(t *testing.T) {
	tl, ok := subagent.NewTool(&fakeSpawner{childID: "c"}, config.Subagents{Enabled: true})
	if !ok {
		t.Fatal("NewTool ok = false")
	}
	tl.Bind("parent-1")

	if _, err := tl.Run(context.Background(), json.RawMessage(`{not json`)); err == nil {
		t.Error("undecodable input did not produce a Go error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tl.Run(ctx, json.RawMessage(`{"prompt":"go"}`)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx err = %v, want context.Canceled", err)
	}
}

// TestRunSpawnsThroughTheSeam is the happy path: the bound session id becomes
// the child's parent, the agent and prompt are forwarded verbatim, and the
// result names the child so the model can refer to it.
func TestRunSpawnsThroughTheSeam(t *testing.T) {
	spawner := &fakeSpawner{childID: "child-9"}
	tl, ok := subagent.NewTool(spawner, config.Subagents{Enabled: true, Agents: []string{"go-developer"}})
	if !ok {
		t.Fatal("NewTool ok = false")
	}
	tl.Bind("parent-7")

	res, err := tl.Run(context.Background(), json.RawMessage(`{"agent":" go-developer ","prompt":"review the diff"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("Run result is an error: %s", res.Content)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("seam calls = %d, want 1", len(spawner.calls))
	}
	// The agent is trimmed BEFORE it reaches the seam — it is forwarded to
	// runner.Options.Agent and stamped onto every one of the child's tool
	// events, where a stray space would silently split the attribution.
	want := spawnCall{parentID: "parent-7", agent: "go-developer", prompt: "review the diff"}
	if spawner.calls[0] != want {
		t.Errorf("seam call = %+v, want %+v", spawner.calls[0], want)
	}
	if !strings.Contains(res.Content, "child-9") {
		t.Errorf("result %q does not name the child session", res.Content)
	}
	if got := res.Metadata.Extra["session_id"]; got != "child-9" {
		t.Errorf("metadata session_id = %v, want child-9", got)
	}
}

// TestDescriptionNamesTheAsyncContract guards the one thing a model cannot
// discover by trying: the call returns before the child finishes, and the
// answer arrives later as a message. A description that omitted it would invite
// the model to sit and wait for a result that never comes back through this
// tool.
func TestDescriptionNamesTheAsyncContract(t *testing.T) {
	tl, ok := subagent.NewTool(&fakeSpawner{}, config.Subagents{Enabled: true, Agents: []string{"go-developer"}})
	if !ok {
		t.Fatal("NewTool ok = false")
	}
	desc := tl.Description()
	for _, want := range []string{"does NOT wait", "go-developer"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description does not mention %q: %s", want, desc)
		}
	}
}
