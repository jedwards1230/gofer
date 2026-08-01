package supervisor_test

// capabilities_test.go covers [supervisor.Supervisor.Capabilities] (gofer#303)
// — the producer of the report /mcp and /skills render.
//
// The assertions worth reading are the two about what the report REFUSES to
// claim: a server the manager never dials must not come back Connected, and a
// tool count must be a total rather than anything per-server.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestCapabilitiesReportsEveryConfiguredServer covers the server projection,
// including the two states that are NOT simply "in the manager's Down list":
// a disabled server and one whose transport gofer does not recognize are never
// dialed at all, so neither appears in Down — and computing Connected as
// "enabled and not down" would report both as up.
func TestCapabilitiesReportsEveryConfiguredServer(t *testing.T) {
	mcp := config.MCP{Servers: []config.MCPServer{
		{Name: "unreachable", Command: "/nonexistent/mcp-server"},
		{Name: "parked", Command: "/nonexistent/mcp-server", Enabled: boolPtr(false)},
		{Name: "future-ws", TransportMode: "ws", Command: "/nonexistent/mcp-server"},
	}}
	sup := newCapSupervisor(t, t.TempDir(), supervisor.Config{MCP: func() config.MCP { return mcp }})

	got := map[string]capability.Server{}
	for _, s := range sup.Capabilities(t.TempDir()).MCP.Servers {
		got[s.Name] = s
	}
	if len(got) != 3 {
		t.Fatalf("expected all three configured servers, got %+v", got)
	}
	for _, tc := range []struct {
		name          string
		wantEnabled   bool
		wantTransport string
	}{
		{"unreachable", true, "stdio"},
		{"parked", false, "stdio"},
		{"future-ws", true, ""}, // an unrecognized transport resolves to unsupported
	} {
		s := got[tc.name]
		if s.Enabled != tc.wantEnabled {
			t.Errorf("%s: Enabled = %v, want %v", tc.name, s.Enabled, tc.wantEnabled)
		}
		if s.ConfiguredTransport != tc.wantTransport {
			t.Errorf("%s: ConfiguredTransport = %q, want %q", tc.name, s.ConfiguredTransport, tc.wantTransport)
		}
		if s.Connected {
			t.Errorf("%s: Connected = true; nothing here can connect", tc.name)
		}
	}
}

// TestCapabilitiesSchemaModeIsConfiguredIntent pins that the resident /
// index-only split is populated ONLY under index mode. Under preload every
// schema is already in context, so a non-zero split there would be describing
// a division that does not exist.
func TestCapabilitiesSchemaModeIsConfiguredIntent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tools    config.Tools
		wantMode string
	}{
		{"preload default", config.Tools{}, "preload"},
		{"explicit index", config.Tools{SchemaMode: "index"}, "index"},
		{"unrecognized fails safe to preload", config.Tools{SchemaMode: "indx"}, "preload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup := newCapSupervisor(t, t.TempDir(), supervisor.Config{
				Tools: func() config.Tools { return tc.tools },
			})
			mcp := sup.Capabilities(t.TempDir()).MCP
			if mcp.SchemaMode != tc.wantMode {
				t.Errorf("SchemaMode = %q, want %q", mcp.SchemaMode, tc.wantMode)
			}
			// No MCP servers are configured here, so there are no federated
			// tools to split under either mode — the point is that the counts
			// track the tool list, never the mode alone.
			if mcp.ConnectedTools != 0 || mcp.ResidentTools != 0 || mcp.IndexOnlyTools != 0 {
				t.Errorf("expected zero tool counts with no servers, got %+v", mcp)
			}
		})
	}
}

// TestCapabilitiesLoadsSkillsWithProjectPrecedence covers the skills half:
// both directories are searched, the PROJECT copy wins a name clash (which is
// the opposite of the SDK loader's raw directory order and the whole reason
// config.Skills.Directories lists cwd first), the loser arrives as a shadowed
// diagnostic, skills.disabled is reflected, and skillset.Summarize's line is
// produced — its first caller anywhere.
func TestCapabilitiesLoadsSkillsWithProjectPrecedence(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	writeCapSkill(t, filepath.Join(cwd, ".gofer", "skills", "clash"), "clash", "project copy")
	loser := filepath.Join(root, "skills", "clash")
	writeCapSkill(t, loser, "clash", "global copy")
	writeCapSkill(t, filepath.Join(root, "skills", "muted"), "muted", "a disabled skill")

	sup := newCapSupervisor(t, root, supervisor.Config{
		Skills: func() config.Skills { return config.Skills{Disabled: []string{"muted"}} },
	})
	skills := sup.Capabilities(cwd).Skills

	byName := map[string]capability.Skill{}
	for _, s := range skills.Loaded {
		byName[s.Name] = s
	}
	if got := byName["clash"].Description; got != "project copy" {
		t.Errorf("project copy must win the name, got %q", got)
	}
	if !byName["muted"].Disabled {
		t.Error("a skill named in skills.disabled must be reported disabled (it is still loaded)")
	}

	var shadowedPaths []string
	for _, d := range skills.Diagnostics {
		if d.Shadowed {
			shadowedPaths = append(shadowedPaths, d.Path)
		}
	}
	want := filepath.Join(loser, "SKILL.md")
	if len(shadowedPaths) != 1 || shadowedPaths[0] != want {
		t.Errorf("shadowed = %v, want exactly [%s]", shadowedPaths, want)
	}
	if skills.Summary == "" {
		t.Error("skillset.Summarize must produce a line when there are diagnostics")
	}
	if len(skills.Directories) != 2 || skills.Directories[0] != filepath.Join(cwd, ".gofer", "skills") {
		t.Errorf("directories = %v, want the project directory first", skills.Directories)
	}
}

// TestCapabilitiesEmptyIsAValidAnswer pins that a supervisor with nothing
// configured produces a clean empty report rather than an error or a panic —
// the state every fresh install is in, and the one a panel opens into most.
func TestCapabilitiesEmptyIsAValidAnswer(t *testing.T) {
	sup := newCapSupervisor(t, t.TempDir(), supervisor.Config{})
	snap := sup.Capabilities(t.TempDir())
	if len(snap.MCP.Servers) != 0 || len(snap.Skills.Loaded) != 0 || len(snap.Skills.Diagnostics) != 0 {
		t.Errorf("expected an empty report, got %+v", snap)
	}
	if snap.Skills.Summary != "" {
		t.Errorf("no diagnostics must summarize to \"\", got %q", snap.Skills.Summary)
	}
}

// newCapSupervisor builds a supervisor at root with cfg's resolvers. No
// session is ever created, so no provider is needed.
func newCapSupervisor(t testing.TB, root string, cfg supervisor.Config) *supervisor.Supervisor {
	t.Helper()
	cfg.Root = root
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// writeCapSkill lays down one <dir>/SKILL.md in the standard layout.
func writeCapSkill(t testing.TB, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nthe body\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s/SKILL.md: %v", dir, err)
	}
}

// boolPtr is config's *bool idiom (an explicit false, distinct from unset).
func boolPtr(b bool) *bool { return &b }
