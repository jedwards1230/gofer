package daemon_test

// capabilities_test.go covers gofer/capabilities (gofer#303) over the real
// wire: the method is reachable at all (it registers itself from an init(),
// not from handlers.go's table literal — see capabilities.go), a supervisor
// that CAN report answers with a snapshot, and one that cannot is reported as
// unsupported rather than as an empty snapshot a client would render as
// "nothing configured".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestCapabilitiesOverTheWire drives the whole path — method registration,
// handler, supervisor projection, client decode — against a daemon hosting a
// real in-process supervisor with two configured MCP servers and a real skills
// directory on disk.
func TestCapabilitiesOverTheWire(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".gofer", "skills", "project-skill"), "project-skill", "a project-scoped skill")
	writeSkill(t, filepath.Join(root, "skills", "global-skill"), "global-skill", "a globally-scoped skill")

	mcpCfg := config.MCP{Servers: []config.MCPServer{
		{Name: "unreachable", Command: "/nonexistent/mcp-server-binary"},
		{Name: "parked", Command: "/nonexistent/other", Enabled: boolPtr(false)},
	}}
	sup := newCapabilitySupervisor(t, root, mcpCfg)
	_, url := newTestDaemon(t, sup, "")

	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	snap, err := c.Capabilities(context.Background(), cwd)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	if len(snap.MCP.Servers) != 2 {
		t.Fatalf("expected both configured servers on the wire, got %+v", snap.MCP.Servers)
	}
	byName := map[string]capability.Server{}
	for _, s := range snap.MCP.Servers {
		byName[s.Name] = s
	}
	// A server pointed at a binary that does not exist can never be connected;
	// the point of asserting it is that "configured" and "connected" are two
	// different fields on the wire, not one.
	if got := byName["unreachable"]; !got.Enabled || got.Connected {
		t.Errorf("unreachable server: want enabled + not connected, got %+v", got)
	}
	if got := byName["parked"]; got.Enabled || got.Connected {
		t.Errorf("parked server: want disabled + not connected, got %+v", got)
	}

	names := map[string]bool{}
	for _, s := range snap.Skills.Loaded {
		names[s.Name] = true
	}
	if !names["project-skill"] || !names["global-skill"] {
		t.Errorf("expected both skills across both directories, got %+v", snap.Skills.Loaded)
	}
	if len(snap.Skills.Directories) != 2 {
		t.Errorf("expected the two-directory discovery order on the wire, got %+v", snap.Skills.Directories)
	}
}

// TestCapabilitiesShadowedDuplicateCrossesTheWire pins the ONE half of skill
// precedence that is recoverable: the losing file. The same name is defined in
// both directories, so the global copy is dropped with a diagnostic — and that
// diagnostic's path is the only place any layer learns which file lost.
//
// Note what is NOT asserted, because it is not knowable: the winning file's
// path. skill.Meta records none.
func TestCapabilitiesShadowedDuplicateCrossesTheWire(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".gofer", "skills", "clash"), "clash", "the project copy, which wins")
	loser := filepath.Join(root, "skills", "clash")
	writeSkill(t, loser, "clash", "the global copy, which loses")

	sup := newCapabilitySupervisor(t, root, config.MCP{})
	_, url := newTestDaemon(t, sup, "")
	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	snap, err := c.Capabilities(context.Background(), cwd)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	if len(snap.Skills.Loaded) != 1 || snap.Skills.Loaded[0].Description != "the project copy, which wins" {
		t.Fatalf("expected the project copy to win, got %+v", snap.Skills.Loaded)
	}
	var shadowed *capability.Diagnostic
	for i, d := range snap.Skills.Diagnostics {
		if d.Shadowed {
			shadowed = &snap.Skills.Diagnostics[i]
		}
	}
	if shadowed == nil {
		t.Fatalf("expected a shadowed diagnostic, got %+v", snap.Skills.Diagnostics)
	}
	if shadowed.Path != filepath.Join(loser, "SKILL.md") {
		t.Errorf("shadowed diagnostic must name the LOSING file, got %q", shadowed.Path)
	}
	if snap.Skills.Summary == "" {
		t.Error("skillset.Summarize's line must reach the wire; got an empty summary")
	}
}

// TestCapabilitiesUnsupportedSupervisor pins the [daemon.CapabilityReporter]
// gap — the `gofer daemon --workers` router's case, modelled here by any
// [daemon.Supervisor] that does not implement the optional interface.
//
// It must come back as [daemon.ErrCapabilitiesUnsupported], NOT as a
// successful empty snapshot: a client that received the latter would render
// "no MCP servers configured" about a daemon that in fact has plenty.
func TestCapabilitiesUnsupportedSupervisor(t *testing.T) {
	d := daemon.New(&settleSupervisor{}, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	c, err := daemon.Dial(context.Background(), "ws"+srv.URL[len("http"):], "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	snap, err := c.Capabilities(context.Background(), t.TempDir())
	if !errors.Is(err, daemon.ErrCapabilitiesUnsupported) {
		t.Fatalf("want ErrCapabilitiesUnsupported, got err=%v snap=%+v", err, snap)
	}
	if len(snap.MCP.Servers) != 0 || len(snap.Skills.Loaded) != 0 {
		t.Errorf("an unsupported answer must carry no snapshot, got %+v", snap)
	}
}

// TestCapabilitiesIsRegisteredWithoutHandlersTable guards the one unusual thing
// about this method's wiring: it is inserted into the router's method table
// from an init() in capabilities.go rather than from handlers.go's map literal.
// Package-level variable initialization completes before any init runs, so that
// is well-defined — but it is exactly the kind of ordering assumption that
// silently stops holding, and the symptom would be a plain method-not-found.
func TestCapabilitiesIsRegisteredWithoutHandlersTable(t *testing.T) {
	sup := newCapabilitySupervisor(t, t.TempDir(), config.MCP{})
	_, url := newTestDaemon(t, sup, "")
	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	raw, err := c.Call(context.Background(), "gofer/capabilities", map[string]string{"cwd": t.TempDir()})
	if err != nil {
		t.Fatalf("gofer/capabilities must be routable: %v", err)
	}
	var res struct {
		Supported bool `json:"supported"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Supported {
		t.Error("an in-process supervisor must report supported=true")
	}
}

// newCapabilitySupervisor builds a supervisor rooted at root with mcp as its
// MCP config. It never creates a session, so it needs no provider — the
// capability report is a pure read of the manager plus the filesystem.
func newCapabilitySupervisor(t testing.TB, root string, mcp config.MCP) *supervisor.Supervisor {
	t.Helper()
	sup, err := supervisor.New(supervisor.Config{
		Root: root,
		MCP:  func() config.MCP { return mcp },
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// writeSkill lays down one <dir>/SKILL.md in the standard layout.
func writeSkill(t testing.TB, dir, name, description string) {
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
