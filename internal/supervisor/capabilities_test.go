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
	"sync/atomic"
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

// TestCapabilitiesDescribeTheManagerNotTheLiveConfig is the regression for the
// fabricated "connected".
//
// In production [supervisor.Config.MCP] is a LIVE re-read of config.json
// (cmd/gofer's mcpConfigResolver), while the connection manager is built once
// at New and its Snapshot.Down only ever iterates that construction-time set.
// Deriving a server list from the live config therefore reported a server added
// AFTER startup as connected: it was enabled, and it was absent from Down for
// the trivial reason that the manager had never heard of it. The panel rendered
// a confident green "connected" for a server that had never been dialed — in
// exactly the situation that sends someone to /mcp.
//
// The resolver here returns a DIFFERENT set on every call after the first,
// which is what the original test's static closure could not express and why
// nothing caught this.
func TestCapabilitiesDescribeTheManagerNotTheLiveConfig(t *testing.T) {
	var calls atomic.Int64
	atStart := config.MCP{Servers: []config.MCPServer{
		{Name: "present-at-start", Command: "/nonexistent/mcp-server"},
	}}
	live := config.MCP{Servers: []config.MCPServer{
		{Name: "present-at-start", Command: "/nonexistent/mcp-server"},
		{Name: "added-later", Command: "/nonexistent/mcp-server"},
	}}
	sup := newCapSupervisor(t, t.TempDir(), supervisor.Config{
		MCP: func() config.MCP {
			if calls.Add(1) == 1 {
				return atStart
			}
			return live
		},
	})

	mcp := sup.Capabilities(t.TempDir()).MCP
	if len(mcp.Servers) != 1 || mcp.Servers[0].Name != "present-at-start" {
		t.Fatalf("the report must describe the manager's own server set, got %+v", mcp.Servers)
	}
	if mcp.Servers[0].Connected {
		t.Error("a server pointed at a nonexistent binary must never read as connected")
	}
	// The drift is not swallowed: omitting the added server is right, but doing
	// so silently would leave an operator who just edited config.json with no
	// explanation for why it is missing.
	if !mcp.ConfigDrifted {
		t.Error("a config.json that gained a server since startup must be reported as drifted")
	}
}

// TestCapabilitiesReportNoDriftWhenConfigIsStable is the negative half: an
// unchanged file must not raise the notice, or it would fire on every open and
// be trained away. Timeout-only edits deliberately do not count — they change
// how the manager behaves, not WHICH servers it holds.
func TestCapabilitiesReportNoDriftWhenConfigIsStable(t *testing.T) {
	ms := 1234
	base := []config.MCPServer{{Name: "steady", Command: "/nonexistent/mcp-server"}}
	var calls atomic.Int64
	sup := newCapSupervisor(t, t.TempDir(), supervisor.Config{
		MCP: func() config.MCP {
			if calls.Add(1) == 1 {
				return config.MCP{Servers: base}
			}
			return config.MCP{Servers: base, ConnectTimeoutMS: &ms}
		},
	})
	if sup.Capabilities(t.TempDir()).MCP.ConfigDrifted {
		t.Error("a timeout-only edit must not be reported as server drift")
	}
}

// TestCapabilitiesWithoutCwdReportsOnlyTheStoreRoot covers the empty-cwd
// request (the wire's cwd is omitempty, so any client that omits it lands
// here). [config.Skills.Directories] joins cwd unconditionally, so "" yields
// the RELATIVE ".gofer/skills" — which would be scanned against whatever
// working directory the daemon happens to have and then reported verbatim as
// though it were the caller's project.
func TestCapabilitiesWithoutCwdReportsOnlyTheStoreRoot(t *testing.T) {
	root := t.TempDir()
	writeCapSkill(t, filepath.Join(root, "skills", "global-only"), "global-only", "a store-root skill")

	skills := newCapSupervisor(t, root, supervisor.Config{}).Capabilities("").Skills
	if len(skills.Directories) != 1 || skills.Directories[0] != filepath.Join(root, "skills") {
		t.Fatalf("an absent cwd must report the store root alone, got %v", skills.Directories)
	}
	for _, dir := range skills.Directories {
		if !filepath.IsAbs(dir) {
			t.Errorf("a relative directory %q would resolve against this process's cwd, not the caller's", dir)
		}
	}
	if len(skills.Loaded) != 1 || skills.Loaded[0].Name != "global-only" {
		t.Errorf("the store-root skill must still load, got %+v", skills.Loaded)
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
