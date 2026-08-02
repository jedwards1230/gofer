package tui

// fixtures_test.go holds the small set of golden-test fixtures shared across
// both this package's internal tests (app_internal_test.go, dialog_color_test.go)
// and the black-box tui_test package (app_test.go, overview_test.go,
// color_layout_test.go) — the standard export_test pattern: this file is only
// compiled for tests, but its exported names are reachable from tui_test the
// same as any other exported package identifier.
//
// It also carries `ingested`, which is unexported on purpose — it hands back a
// Model, so only this package's internal tests can do anything with one.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// BenchSessionID is the session id every transcript benchmark's fixture events
// carry, shared so the fixture and the App the benchmark drives agree on it.
const BenchSessionID = "0192a1b2-c3d4-7e5f-8a90-000000000001"

// GoldenBenchTurn returns ONE realistic turn's worth of events: a user message,
// an assistant reply carrying prose and a fenced code block, and a tool call
// with a multi-line result.
//
// It exists because every transcript benchmark used to ingest a single line of
// plain text per message (`turn %d: ...`), and a benchmark's fixture decides
// what its number means (gofer#315, item 3). Plain one-line text skips the
// markdown path almost entirely: no fenced block to highlight, no tool-result
// block to lay out, no wrapping. Measured against the same render, realistic
// content costs roughly 8x the allocations and 11x the bytes of the plain
// fixture — so every absolute figure in bench/baseline.txt was that optimistic,
// and a regression confined to markdown, code-block or tool-result rendering
// barely moved the gated number.
//
// The content varies with i so the markdown memo (markdown.go) cannot serve
// every turn from one cache entry, which is what a real conversation looks
// like. It is not RANDOM: the benchmark must stay byte-identical run to run for
// its allocation counts to be comparable at all.
func GoldenBenchTurn(i int) []event.Event {
	input, _ := json.Marshal(map[string]string{
		"path":    fmt.Sprintf("internal/daemon/listener_%d.go", i),
		"pattern": "func (l *Listener) Accept",
	})
	return []event.Event{
		event.NewMessageFinished(BenchSessionID, event.MessageUser,
			fmt.Sprintf("turn %d: the websocket listener accepts the upgrade but never forwards session events to the subscriber — can you wire the fan-out and cover the late-subscriber case?", i)),
		event.NewToolCallStarted(BenchSessionID, fmt.Sprintf("call-%d", i), "grep", input),
		event.NewToolCallFinished(BenchSessionID, fmt.Sprintf("call-%d", i), input,
			fmt.Sprintf(`internal/daemon/listener_%d.go:41:func (l *Listener) Accept(ctx context.Context) error {
internal/daemon/listener_%d.go:52:	conn, err := l.upgrader.Upgrade(w, r, nil)
internal/daemon/listener_%d.go:58:	if err != nil {
internal/daemon/listener_%d.go:59:		return fmt.Errorf("daemon: upgrade: %%w", err)
internal/daemon/listener_%d.go:60:	}
internal/daemon/listener_%d.go:64:	sub := l.broker.Subscribe(event.FilterAll, 64)
internal/daemon/listener_%d.go:65:	defer sub.Close()
internal/daemon/listener_%d.go:71:	for ev := range sub.C() {
internal/daemon/listener_%d.go:72:		if err := conn.WriteJSON(envelope(ev)); err != nil {
internal/daemon/listener_%d.go:73:			return err
internal/daemon/listener_%d.go:74:		}
internal/daemon/listener_%d.go:75:	}`, i, i, i, i, i, i, i, i, i, i, i, i),
			false, nil),
		event.NewMessageFinished(BenchSessionID, event.MessageText,
			fmt.Sprintf(`Found it. `+"`Accept`"+` subscribes **after** the upgrade completes, so anything the
session published between the client's connect and that subscribe is dropped —
which is exactly the late-subscriber case you hit on turn %d.

The fix is to subscribe before the upgrade and hand the subscription to the
pump:

`+"```go"+`
func (l *Listener) Accept(ctx context.Context) error {
	sub := l.broker.Subscribe(event.FilterAll, %d)
	conn, err := l.upgrader.Upgrade(w, r, nil)
	if err != nil {
		sub.Close()
		return fmt.Errorf("daemon: upgrade: %%%%w", err)
	}
	return l.pump(ctx, conn, sub)
}
`+"```"+`

That closes the window without buffering unboundedly: the broker's retained
backlog covers the replay, and the subscription's own buffer bounds the rest.`, i, 64+i)),
	}
}

// GoldenBenchTurns is [GoldenBenchTurn] repeated, in order — the whole
// transcript a size-sweeping benchmark ingests.
func GoldenBenchTurns(turns int) []event.Event {
	evs := make([]event.Event, 0, turns*4)
	for i := range turns {
		evs = append(evs, GoldenBenchTurn(i)...)
	}
	return evs
}

// ingested returns a Model rendered through th with evs applied in order. It
// replaces the chained New(th).Ingest(a).Ingest(b) form, which [Model.Ingest]'s
// pointer receiver deliberately no longer supports as an expression — see its
// doc for why the value-returning shape had to go (gofer#308).
func ingested(th theme.Theme, evs ...event.Event) Model {
	m := New(th)
	for _, e := range evs {
		m.Ingest(e)
	}
	return m
}

// GoldenNow is the fixed reference instant every golden-test fixture ages its
// sessions against, so relative-age output (humanAge/humanDuration) is
// deterministic across machines and CI.
var GoldenNow = time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)

// GoldenTrace returns the shared permission-decision trace every approval
// fixture requests with: the exact two-entry shape the SDK's loop.RuleGuard
// emits for the commonest gated call — nothing matched, and the host can't
// sandbox it either (see loop/guard.go's Evaluate/containOrAsk). Fixtures use
// the real shape, not a made-up string, because the approval prompt's
// rationale is DERIVED from it — a fixture the parser can't read would render
// the "could not determine why" fallback in every golden and hide the feature
// under test.
func GoldenTrace() []string {
	return []string{"rule: unmatched", "containable: false (no container configured)"}
}

// GoldenMeta returns the shared OverviewMeta the App/Overview golden tests
// build through.
func GoldenMeta() OverviewMeta {
	return OverviewMeta{App: "gofer", Version: "0.3.0", Model: "claude-sonnet-5", Cwd: "~/orchestration", Now: GoldenNow}
}

// GoldenCommandEnv returns the shared [CommandEnv] the App/command-panel
// golden tests build through: fixed version/cwd/root and the
// auth-independence default (zero providers authenticated, no persisted
// config) — the state every panel view must open cleanly in. SaveConfig is a
// no-op (never touches disk) — it exists so /config's edit paths exercise a
// non-nil closure in golden tests without leaving files behind; tests that
// need to observe what was written supply their own CommandEnv (see
// config_view_test.go).
func GoldenCommandEnv() CommandEnv {
	return CommandEnv{
		Version:    "0.3.0",
		Cwd:        "~/orchestration",
		Root:       "~/.gofer",
		Auth:       func() ([]ProviderAuth, error) { return nil, nil },
		Config:     func() (config.Config, error) { return config.Config{}, nil },
		SaveConfig: func(config.Config) error { return nil },
		Capabilities: func(context.Context) (capability.Answer, error) {
			return capability.Answer{Known: true, Snapshot: GoldenCapabilities()}, nil
		},
	}
}

// GoldenCapabilities returns the shared [capability.Snapshot] the /mcp and
// /skills golden tests render. It is deliberately the AWKWARD case rather than
// the happy one, because the happy one proves almost nothing: every distinct
// server state the MCP tab has a different word for is present exactly once
// (connected, down, an unrecognized transport, disabled), the tool surface is
// in index mode so the resident/index-only split renders at all, and the
// skills half carries a shadowed duplicate, a size-skipped candidate, a
// disabled skill, and a truncated description.
//
// Every path here is a literal "~/..." string, never a real one: the goldens
// must render identically on every machine, and these values are shown
// verbatim.
func GoldenCapabilities() capability.Snapshot {
	return capability.Snapshot{
		MCP: capability.MCP{
			Servers: []capability.Server{
				{Name: "github", ConfiguredTransport: "stdio", Enabled: true, Connected: true},
				{Name: "linear", ConfiguredTransport: "http", Enabled: true},
				{Name: "legacy-ws", Enabled: true},
				{Name: "scratch", ConfiguredTransport: "stdio"},
			},
			ConnectedTools: 7,
			SchemaMode:     "index",
			ResidentTools:  1,
			IndexOnlyTools: 6,
		},
		Skills: capability.Skills{
			Directories: []string{"~/orchestration/.gofer/skills", "~/.gofer/skills"},
			Loaded: []capability.Skill{
				{Name: "commit-msg", Description: "Write a conventional-commit message from a staged diff"},
				{Name: "deep-dive", Description: "Trace a symbol across packages before changing it", Truncated: true},
				{Name: "release", Description: "Cut a release and draft its notes", Disabled: true},
			},
			Diagnostics: []capability.Diagnostic{
				{
					Path:     "~/.gofer/skills/commit-msg/SKILL.md",
					Detail:   `skill: duplicate name "commit-msg"; the earlier directory's definition wins`,
					Shadowed: true,
				},
				{
					Path:   "~/.gofer/skills/whole-repo/SKILL.md",
					Detail: "skill: body exceeds 262144 bytes",
				},
			},
			Summary: `skills: skipped ~/.gofer/skills/commit-msg/SKILL.md: skill: duplicate name "commit-msg"; the earlier directory's definition wins (+1 more)`,
		},
	}
}

// promptScopeEnv is GoldenCommandEnv with session.prompt_esc_scope set to
// "prompt" — the opt-in where Esc cancels only the focused prompt (an ask_user
// decision resolves cancelled, an approval is denied) instead of interrupting
// the whole turn. The dialog tests that assert the opt-in behavior build
// through it; every other test keeps the default (turn) scope.
func promptScopeEnv() CommandEnv {
	env := GoldenCommandEnv()
	env.Config = func() (config.Config, error) {
		return config.Config{Session: config.Session{PromptEscScope: string(config.PromptEscScopePrompt)}}, nil
	}
	return env
}

// GoldenRoster returns the two-session fixture the App golden and behavioral
// tests navigate: a working session (selected first — most recently active)
// and an idle one awaiting input.
func GoldenRoster() []SessionInfo {
	return []SessionInfo{
		{
			ID:      "0192a1b2-app0-7000-8000-000000000001",
			Title:   "wire the app root",
			Summary: "overview <-> peek <-> attach nav",
			Status:  StatusWorking,
			Cost:    provider.Cost{USD: 0.1120},
			// Usage is populated (row 2 leaves it zero-valued) so the /usage and
			// /stats panel goldens render real token numbers, and the Stats
			// rollup sums a mixed populated/zero roster.
			Usage:   provider.Usage{InputTokens: 18234, OutputTokens: 4096, CacheReadTokens: 12000, CacheWriteTokens: 512},
			Created: GoldenNow.Add(-15 * time.Minute),
			Updated: GoldenNow.Add(-2 * time.Minute),
		},
		{
			ID:      "0192a1b2-app0-7000-8000-000000000002",
			Title:   "review the supervisor contract",
			Summary: "turn finished — awaiting the next prompt",
			Status:  StatusNeedsInput,
			Cost:    provider.Cost{USD: 0.0450},
			Created: GoldenNow.Add(-30 * time.Minute),
			Updated: GoldenNow.Add(-5 * time.Minute),
		},
	}
}
