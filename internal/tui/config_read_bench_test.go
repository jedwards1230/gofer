package tui

// config_read_bench_test.go measures what a frame costs when its config reads
// are REAL, which is the gap gofer#315 item 2 names.
//
// The gap. [GoldenCommandEnv] wires Config to `func() (config.Config, error) {
// return config.Config{}, nil }` — a closure that allocates nothing and touches
// no disk. Every App golden test builds through it, and so did
// [BenchmarkAppRenderMassiveTranscript]: the one benchmark that renders a whole
// App frame was measuring a frame in which every config read was free.
//
// In the shipped app it is not free. The TUI resolves several settings by
// calling the Config closure on EVERY use rather than sampling it once —
// deliberately, so an edit from /config or from another attached client takes
// effect on the next frame ("always current, never a stale snapshot"). That
// costs three calls per drawn frame (five with an active selection), one per
// streamed event, and six per drag-motion Update+View. Behind each call, in
// cmd/gofer, sits config.Load: os.ReadFile + json.Unmarshal + validate.
//
// So the axis here is CONFIG BYTES — which genuinely accumulate, since MCP
// servers, LSP servers, permission rules and skills paths all live in that one
// file — and the benchmark sweeps it against both loaders:
//
//   - loader=direct is config.Load per call: what shipped before this change,
//     and the reference the memo is judged against.
//   - loader=cached is [config.CachedLoader]: what ships now. It re-reads
//     whenever the file's mtime or size moves, so the contract above survives;
//     an unchanged file costs one stat instead of a read+parse+validate.
//
// Both rows are gated. The direct rows are what makes a regression in the
// config read itself visible at all (a new validation pass, a bigger default
// section); the cached rows gate the memo's own per-frame cost.

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// benchConfigSmall is a config the size a real gofer install actually has: a
// couple of session defaults and nothing else.
func benchConfigSmall() config.Config {
	return config.Config{
		Session: config.Session{Model: "claude-sonnet-5", Effort: "medium"},
		TUI:     config.TUI{},
	}
}

// benchConfigLarge is the shape a heavily-configured install reaches: 100
// permission rules and 20 MCP servers. Not a stress fixture invented to look
// bad — a permission ruleset grows one rule at a time as an operator answers
// "allow, remember", and gofer federates one entry per MCP server.
func benchConfigLarge() config.Config {
	c := benchConfigSmall()
	for i := range 100 {
		c.Permissions = append(c.Permissions, config.Rule{
			Verdict:   "allow",
			Tool:      "bash",
			Specifier: fmt.Sprintf("prefix:git log --oneline -- internal/service_%02d/", i),
		})
	}
	for i := range 20 {
		c.MCP.Servers = append(c.MCP.Servers, config.MCPServer{
			Name:    fmt.Sprintf("server_%02d", i),
			Command: "/usr/local/bin/mcp-server",
			Args:    []string{"--transport", "stdio", "--root", fmt.Sprintf("/srv/mcp/%02d", i)},
			Env:     map[string]config.SecretRef{"MCP_TOKEN": config.SecretRef(fmt.Sprintf("env:MCP_TOKEN_%02d", i))},
		})
	}
	return c
}

// benchConfigEnv writes cfg to a temp config.json and returns a CommandEnv
// whose Config closure reads it for real, the file's size so the benchmark can
// report the axis it is sweeping, and a counter of how many times the closure
// has been called.
//
// The counter is not decoration. The defect this benchmark exists to correct is
// a Config closure that costs nothing, and there are two ways to have one: the
// closure does not read the file (checked below), or the RENDER does not call
// the closure. The second is the one a numbers-only check cannot see — a render
// that stopped reading config would look like a large, welcome improvement. See
// [assertRenderReadsConfig].
func benchConfigEnv(b *testing.B, cfg config.Config, cached bool) (CommandEnv, int64, *atomic.Int64) {
	b.Helper()
	root := b.TempDir()
	path := config.DefaultPath(root)
	if err := config.Save(path, cfg); err != nil {
		b.Fatalf("save config: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		b.Fatalf("stat config: %v", err)
	}

	load := func() (config.Config, error) { return config.Load(path) }
	if cached {
		load = config.CachedLoader(path)
	}
	var reads atomic.Int64
	env := GoldenCommandEnv()
	env.Root = root
	env.Config = func() (config.Config, error) {
		reads.Add(1)
		return load()
	}

	// Prove the closure reads the file that was just written, rather than
	// failing loudly or — the case this whole benchmark exists because of —
	// quietly returning an empty struct. Both failure modes still render a
	// frame and still report a number; neither reports the number claimed here.
	// Every consumer of Config in App swallows an error into its default, so
	// nothing downstream would complain either.
	got, err := env.Config()
	if err != nil {
		b.Fatalf("config closure: %v", err)
	}
	if len(got.Permissions) != len(cfg.Permissions) || len(got.MCP.Servers) != len(cfg.MCP.Servers) {
		b.Fatalf("config closure returned %d rules / %d MCP servers, want %d / %d — it is not reading the fixture on disk",
			len(got.Permissions), len(got.MCP.Servers), len(cfg.Permissions), len(cfg.MCP.Servers))
	}
	return env, fi.Size(), &reads
}

// minConfigReadsPerFrame is the floor [assertRenderReadsConfig] holds render()
// to.
//
// It is TWO, not the three gofer#315 reports, and the difference is which
// function is being counted. render() resolves tui.approval_body_lines and
// tui.approval_min_transcript_rows, both via promptModel. mouseEnabled's read
// is the third, and it lives in View() — one level ABOVE render. Counted
// through this package's own App with a counting closure:
//
//	render()                  2 reads
//	View()                    3 reads
//	Update(streamed event)    1 read
//
// The issue's figures are right for View and Update; this benchmark measures
// render, which is what [BenchmarkAppRenderMassiveTranscript] has always
// measured, so its floor is render's.
//
// A floor, not an equality: driving this number DOWN is a legitimate
// optimisation (that is what gofer#315 item 2 is about), and a check that
// failed when someone removed a redundant read would be an obstacle rather
// than a guard. What must never happen is it reaching zero, which is the state
// the benchmark was silently in before.
const minConfigReadsPerFrame = 2

// assertRenderReadsConfig fails the benchmark unless ONE render() actually
// calls the Config closure. Run before the timer starts, so it costs the
// measurement nothing.
func assertRenderReadsConfig(b *testing.B, a App, reads *atomic.Int64) {
	b.Helper()
	before := reads.Load()
	if a.render() == "" {
		b.Fatal("rendered an empty frame")
	}
	if got := reads.Load() - before; got < minConfigReadsPerFrame {
		b.Fatalf("one render() made %d config reads, want at least %d — this benchmark claims to measure a frame whose config reads are REAL, and a render that does not read config at all is exactly the state it was written to correct",
			got, minConfigReadsPerFrame)
	}
}

// BenchmarkAppRenderConfigSize renders one frame per iteration against a real
// on-disk config, sweeping the config's size and the loader.
func BenchmarkAppRenderConfigSize(b *testing.B) {
	cases := []struct {
		name string
		cfg  config.Config
	}{
		{"small", benchConfigSmall()},
		{"large", benchConfigLarge()},
	}
	for _, tc := range cases {
		for _, cached := range []bool{false, true} {
			loader := "direct"
			if cached {
				loader = "cached"
			}
			b.Run(fmt.Sprintf("config=%s/loader=%s", tc.name, loader), func(b *testing.B) {
				env, size, reads := benchConfigEnv(b, tc.cfg, cached)
				b.Logf("config.json is %d bytes", size)

				a := newBenchApp(b, env, 50)
				assertRenderReadsConfig(b, a, reads)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if a.render() == "" {
						b.Fatal("rendered an empty frame")
					}
				}
			})
		}
	}
}

// newBenchApp builds an App attached to a session with turns turns of
// [GoldenBenchTurns] content already ingested, sized to the standard test
// terminal. Shared by the render benchmarks in this package.
func newBenchApp(b *testing.B, env CommandEnv, turns int) App {
	b.Helper()
	meta := GoldenMeta()
	meta.AttachSessionID = BenchSessionID
	a := NewApp(theme.Test(), &internalFakeSup{}, meta, env)
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	for _, ev := range GoldenBenchTurns(turns) {
		mdl, _ = a.Update(sessEventMsg{id: BenchSessionID, ev: ev})
		a = mdl.(App)
	}
	return a
}
