package tui_test

// capabilities_panel_test.go covers the /mcp and /skills command-panel tabs
// (gofer#303) end to end, through App's exported surface.
//
// The load-bearing test here is
// TestDaemonUnknownCapabilitiesNeverRendersLocalSnapshot. Every other
// assertion in this file would still pass against a view that quietly read
// [tui.CommandEnv.Config] and the local filesystem when the backend could not
// answer — which is precisely the failure mode these panels exist to avoid,
// because its output looks completely correct.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// newPanelApp builds an App over env exactly as newTestApp does over
// [tui.GoldenCommandEnv], so a test can vary the env — the only input these
// two tabs read — without touching the shared helper.
func newPanelApp(t *testing.T, env tui.CommandEnv) tea.Model {
	t.Helper()
	var m tea.Model = tui.NewApp(theme.Test(), newFakeSup(tui.GoldenRoster()), tui.GoldenMeta(), env)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() returned a nil Cmd; expected the roster fetch")
	}
	m, _ = m.Update(cmd())
	return m
}

// TestGoldenPanelMCP verifies /mcp opens the command panel on the MCP tab and
// pins its body against [tui.GoldenCapabilities]'s deliberately awkward
// fixture (one connected, one down, one unrecognized transport, one disabled).
func TestGoldenPanelMCP(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/mcp")
	testkit.AssertGolden(t, "app_panel_mcp", content(m))
}

// TestGoldenPanelSkills verifies /skills opens the command panel on the Skills
// tab, including the shadowed duplicate, the size-skipped candidate, and
// [skillset.Summarize]'s line — which reaches a human here for the first time.
func TestGoldenPanelSkills(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/skills")
	testkit.AssertGolden(t, "app_panel_skills", content(m))
}

// TestGoldenPanelMCPUnknown pins the UNKNOWN body — the state a
// `gofer daemon --workers` router and any pre-gofer/capabilities daemon both
// produce. It is a golden of its own precisely because the distinction from
// the populated frame above must live in TEXT: these files render
// termenv.Ascii and cannot see a single byte of colour.
func TestGoldenPanelMCPUnknown(t *testing.T) {
	m := newPanelApp(t, unknownCapabilityEnv())
	m = dispatchSlash(t, m, "/mcp")
	testkit.AssertGolden(t, "app_panel_mcp_unknown", content(m))
}

// TestGoldenPanelSkillsUnknown is TestGoldenPanelMCPUnknown's twin for the
// Skills tab.
func TestGoldenPanelSkillsUnknown(t *testing.T) {
	m := newPanelApp(t, unknownCapabilityEnv())
	m = dispatchSlash(t, m, "/skills")
	testkit.AssertGolden(t, "app_panel_skills_unknown", content(m))
}

// unknownCapabilityEnv is a DAEMON-BACKED env whose capability closure answers
// UNKNOWN — the shape [classifyCapabilities] produces for a `--workers` router
// or a daemon predating gofer/capabilities.
func unknownCapabilityEnv() tui.CommandEnv {
	env := tui.GoldenCommandEnv()
	env.DaemonBacked = true
	env.Capabilities = func(context.Context) (capability.Answer, error) {
		return capability.Answer{}, nil
	}
	return env
}

// TestMCPPanelReflectsBackendState is the state-reflection test beside the
// golden: it asserts the MCP tab renders the ANSWER it was handed rather than
// a fixed frame, and that each server state gets its own word (an Ascii golden
// is blind to colour, so a distinction carried only by style would be invisible
// to every test in this package).
func TestMCPPanelReflectsBackendState(t *testing.T) {
	env := tui.GoldenCommandEnv()
	env.Capabilities = func(context.Context) (capability.Answer, error) {
		return capability.Answer{Known: true, Snapshot: capability.Snapshot{
			MCP: capability.MCP{
				Servers: []capability.Server{
					{Name: "alpha", ConfiguredTransport: "stdio", Enabled: true, Connected: true},
					{Name: "bravo", ConfiguredTransport: "http", Enabled: true},
					{Name: "charlie", Enabled: true},
					{Name: "delta", ConfiguredTransport: "stdio"},
				},
				ConnectedTools: 4,
				SchemaMode:     "index",
				ResidentTools:  1,
				IndexOnlyTools: 3,
			},
		}}, nil
	}

	got := content(dispatchSlash(t, newPanelApp(t, env), "/mcp"))
	for _, want := range []string{
		"MCP servers: 1 connected of 4 configured",
		"alpha",
		"connected",
		"bravo",
		"not connected",
		"charlie",
		"unsupported transport",
		"delta",
		"disabled",
		"Federated tools: 4 total across connected servers",
		"index-only",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("MCP panel missing %q:\n%s", want, got)
		}
	}
}

// TestMCPPanelReportsNoServersDistinctlyFromUnknown pins the distinction the
// whole [capability.Answer] two-level shape exists for: a backend that
// answered "nothing configured" and one that could not answer at all must not
// read alike.
func TestMCPPanelReportsNoServersDistinctlyFromUnknown(t *testing.T) {
	empty := tui.GoldenCommandEnv()
	empty.Capabilities = func(context.Context) (capability.Answer, error) {
		return capability.Answer{Known: true}, nil
	}

	none := content(dispatchSlash(t, newPanelApp(t, empty), "/mcp"))
	unknown := content(dispatchSlash(t, newPanelApp(t, unknownCapabilityEnv()), "/mcp"))

	// The CLAIM form, not the substring: the unknown body mentions the phrase
	// "none configured" precisely to deny it, so asserting on the bare phrase
	// would be an assertion about wording rather than about meaning.
	const noneClaim = "MCP servers: none configured."
	if !strings.Contains(none, noneClaim) {
		t.Errorf("a known-empty answer must say so plainly, got:\n%s", none)
	}
	if strings.Contains(none, "UNKNOWN") {
		t.Errorf("a known-empty answer must not read as unknown, got:\n%s", none)
	}
	if !strings.Contains(unknown, "UNKNOWN") {
		t.Errorf("an unknown answer must say UNKNOWN, got:\n%s", unknown)
	}
	if strings.Contains(unknown, noneClaim) {
		t.Errorf("an unknown answer must not claim none configured, got:\n%s", unknown)
	}
}

// TestSkillsPanelReflectsBackendState is the Skills tab's state-reflection
// twin, covering the two markers that must be words rather than styling
// (disabled, truncated), the shadowed duplicate's losing path, and
// [skillset.Summarize]'s line reaching the screen.
func TestSkillsPanelReflectsBackendState(t *testing.T) {
	env := tui.GoldenCommandEnv()
	env.Capabilities = func(context.Context) (capability.Answer, error) {
		return capability.Answer{Known: true, Snapshot: capability.Snapshot{
			Skills: capability.Skills{
				Loaded: []capability.Skill{
					{Name: "alpha-skill", Description: "does alpha things"},
					{Name: "bravo-skill", Description: "does bravo things", Disabled: true},
					{Name: "charlie-skill", Description: "does charlie things", Truncated: true},
				},
				Diagnostics: []capability.Diagnostic{
					{Path: "/globals/alpha-skill/SKILL.md", Detail: "dup", Shadowed: true},
					{Path: "/globals/broken/SKILL.md", Detail: "no frontmatter"},
				},
				Summary: "skills: skipped /globals/alpha-skill/SKILL.md: dup (+1 more)",
			},
		}}, nil
	}

	got := content(dispatchSlash(t, newPanelApp(t, env), "/skills"))
	for _, want := range []string{
		"Skills: 3 loaded, 1 disabled, 2 skipped",
		"alpha-skill",
		"(disabled)",
		"(truncated)",
		"shadowed",
		"/globals/alpha-skill/SKILL.md",
		"no frontmatter",
		"skills: skipped /globals/alpha-skill/SKILL.md: dup (+1 more)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Skills panel missing %q:\n%s", want, got)
		}
	}
}

// TestDaemonUnknownCapabilitiesNeverRendersLocalSnapshot is the test that
// catches the lie.
//
// The setup is everything a fallback would need and want: the env is
// DAEMON-BACKED, its capability closure answers UNKNOWN, its Config closure
// returns a config carrying two real MCP servers, and a genuine skills
// directory exists on disk with a genuine SKILL.md in it. A view that reached
// for either local source when the backend could not answer would produce a
// full, confident, entirely plausible panel — describing this machine while
// claiming to describe the daemon's.
//
// So the assertion is negative on purpose: the rendered panel must say UNKNOWN
// and must not contain ANY of those locally-available names. Asserting only
// "it says UNKNOWN" would pass against a view that printed the local server
// list underneath the word.
func TestDaemonUnknownCapabilitiesNeverRendersLocalSnapshot(t *testing.T) {
	cwd := t.TempDir()
	skillDir := filepath.Join(cwd, ".gofer", "skills", "local-only-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	body := "---\nname: local-only-skill\ndescription: a skill that exists only on this machine\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	env := tui.GoldenCommandEnv()
	env.Cwd = cwd
	env.Root = cwd
	env.DaemonBacked = true
	env.Config = func() (config.Config, error) {
		return config.Config{MCP: config.MCP{Servers: []config.MCPServer{
			{Name: "local-only-github", Command: "npx"},
			{Name: "local-only-linear", URL: "https://example.invalid/mcp"},
		}}}, nil
	}
	// The daemon cannot answer — a --workers router, or a daemon older than
	// gofer/capabilities. Both arrive here as this exact value.
	env.Capabilities = func(context.Context) (capability.Answer, error) {
		return capability.Answer{}, nil
	}

	// Names reachable ONLY by a local read. None may appear on either tab.
	leaks := []string{"local-only-github", "local-only-linear", "local-only-skill"}

	for _, tc := range []struct{ command, tab string }{
		{"/mcp", "MCP"},
		{"/skills", "Skills"},
	} {
		t.Run(tc.tab, func(t *testing.T) {
			got := content(dispatchSlash(t, newPanelApp(t, env), tc.command))
			if !strings.Contains(got, "UNKNOWN") {
				t.Fatalf("%s tab must say UNKNOWN when the daemon cannot answer, got:\n%s", tc.tab, got)
			}
			for _, leak := range leaks {
				if strings.Contains(got, leak) {
					t.Errorf("%s tab rendered LOCAL state %q while attached to a daemon that answered unknown:\n%s", tc.tab, leak, got)
				}
			}
		})
	}
}

// TestCapabilityFetchErrorRendersUnknown pins the collapse rule: a transport
// failure is not something the user can act on, so it becomes UNKNOWN rather
// than an error banner — and, critically, not an empty list either.
func TestCapabilityFetchErrorRendersUnknown(t *testing.T) {
	env := tui.GoldenCommandEnv()
	env.Capabilities = func(context.Context) (capability.Answer, error) {
		return capability.Answer{Known: true, Snapshot: tui.GoldenCapabilities()}, errors.New("connection reset")
	}

	got := content(dispatchSlash(t, newPanelApp(t, env), "/mcp"))
	if !strings.Contains(got, "UNKNOWN") {
		t.Errorf("a failed fetch must render UNKNOWN, got:\n%s", got)
	}
	// The closure handed back a populated snapshot ALONGSIDE the error. A
	// caller that read it anyway would render a perfectly good-looking panel
	// from an answer that was never successfully retrieved.
	if strings.Contains(got, "github") {
		t.Errorf("a failed fetch must discard its snapshot, got:\n%s", got)
	}
}

// TestNilCapabilitiesSourceRendersUnknownNotLoading covers the zero
// [tui.CommandEnv] — the wiring cmd/gofer's shared buildCommandEnv leaves
// behind, and what several existing tests construct. With no source there is
// nothing to wait for, so the panel must settle on UNKNOWN rather than sit on
// a "Loading…" line forever.
func TestNilCapabilitiesSourceRendersUnknownNotLoading(t *testing.T) {
	env := tui.GoldenCommandEnv()
	env.Capabilities = nil

	got := content(dispatchSlash(t, newPanelApp(t, env), "/skills"))
	if !strings.Contains(got, "UNKNOWN") {
		t.Errorf("a nil capability source must render UNKNOWN, got:\n%s", got)
	}
	if strings.Contains(got, "Loading") {
		t.Errorf("a nil capability source must never render a load that cannot resolve, got:\n%s", got)
	}
}

// TestCapabilityTabsShareOneFetch pins that the two tabs are fed by ONE fetch:
// opening /mcp and tabbing right into Skills must not issue a second round
// trip, and the Skills tab must render the answer the MCP open already
// obtained rather than its own UNKNOWN.
func TestCapabilityTabsShareOneFetch(t *testing.T) {
	calls := 0
	env := tui.GoldenCommandEnv()
	env.Capabilities = func(context.Context) (capability.Answer, error) {
		calls++
		return capability.Answer{Known: true, Snapshot: tui.GoldenCapabilities()}, nil
	}

	m := dispatchSlash(t, newPanelApp(t, env), "/mcp")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // MCP -> Skills

	if calls != 1 {
		t.Errorf("expected exactly one capability fetch across both tabs, got %d", calls)
	}
	got := content(m)
	if !strings.Contains(got, "[Skills]") {
		t.Fatalf("expected the Skills tab to be active, got:\n%s", got)
	}
	if !strings.Contains(got, "commit-msg") {
		t.Errorf("the Skills tab must reuse the answer /mcp already fetched, got:\n%s", got)
	}
}

// TestCapabilityTabsAreRegisteredCommands pins the dispatcher wiring: both
// commands resolve and both are offered by autocomplete (which is driven off
// [tui.Registry.List], so a Hidden or unregistered command would silently
// vanish from the popup).
func TestCapabilityTabsAreRegisteredCommands(t *testing.T) {
	for _, tc := range []struct{ command, tab string }{
		{"/mcp", "[MCP]"},
		{"/skills", "[Skills]"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got := content(dispatchSlash(t, newTestApp(t, newFakeSup(tui.GoldenRoster())), tc.command))
			if !strings.Contains(got, tc.tab) {
				t.Errorf("%s must open the panel on %s, got:\n%s", tc.command, tc.tab, got)
			}
		})
	}

	// The autocomplete popup filters [Registry.List]; typing a prefix must
	// offer each command with no wiring of its own. Filtered rather than a bare
	// "/" on purpose — the unfiltered popup scrolls, so a bare "/" would test
	// where a row happens to land in the list rather than whether it is offered
	// at all.
	for _, tc := range []struct{ typed, want string }{
		{"/mc", "/mcp"},
		{"/sk", "/skills"},
	} {
		t.Run("autocomplete "+tc.typed, func(t *testing.T) {
			got := content(type_(t, newTestApp(t, newFakeSup(tui.GoldenRoster())), tc.typed))
			if !strings.Contains(got, tc.want) {
				t.Errorf("command autocomplete must offer %s, got:\n%s", tc.want, got)
			}
		})
	}
}
