package supervisor_test

// toolindex_wiring_test.go covers M7 workstream 4's supervisor-side wiring:
// the web_search registration gate, the preload/index toggle actually
// reaching sessionGuard, tool_search promoting for the NEXT turn, Resume
// rehydrating an index from folded history, and the decorator order (index
// outermost, still reaching the sandboxed/diagnosed tool underneath).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/subagent"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// newToolsHarness is [newLSPHarness]'s Tools/Search-axis twin: a harness
// whose supervisor.Config injects the given resolvers, leaving
// PermissionMode/LSP at their package defaults (ask, LSP enabled).
func newToolsHarness(t *testing.T, tools func() config.Tools, search func() config.Search) *harness {
	t.Helper()
	h := &harness{t: t, root: t.TempDir(), sessions: make(map[string]*fakeSession)}

	var nextID int64
	cfg := supervisor.Config{
		Root:   h.root,
		Tools:  tools,
		Search: search,
		// Read through the harness, not captured by value, so a test can set
		// h.subagents after construction and still have it reach the next
		// session — see the field's doc.
		SubagentsConfig: func() config.Subagents { return h.subagents },
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := "sess-" + strconv.FormatInt(atomic.AddInt64(&nextID, 1), 10)
			fs := h.register(id, opts.Cwd)
			fs.approver = opts.Approver
			fs.tools = opts.Tools
			return fs, nil
		},
	}

	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	h.sup = sup
	t.Cleanup(func() { _ = sup.Close() })
	return h
}

// specNames returns reg.Specs()'s tool names, in the order Specs itself
// returns them (resident, then promoted — see toolindex.Index.Specs).
func specNames(reg loop.ToolRegistry) []string {
	specs := reg.Specs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// createSession is this file's shared Create call: every test here only
// cares about the injected registry, never a real turn.
func createSession(t *testing.T, h *harness) *fakeSession {
	t.Helper()
	info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return h.session(info.ID)
}

// TestSessionGuard_PreloadModeByteIdentical proves the fail-safe default
// (zero config.Tools, zero config.Search, zero config.Subagents) produces
// EXACTLY the registry gofer built before these features existed: the 8 SDK
// builtins plus ask_user, no tool_search, no web_search, no spawn_subagent.
// Preload mode must never add a tool to a session's Specs() that an
// unconfigured operator did not have before — that is what makes this round
// purely additive.
//
// The spawn tool is checked BOTH ways deliberately: absent from Specs() (what
// the model is shown, and what the request's prompt-cache prefix is derived
// from) AND unresolvable through Get() (what the model could still CALL). A
// registry that advertised nothing but resolved the name would leave the
// capability quietly reachable.
func TestSessionGuard_PreloadModeByteIdentical(t *testing.T) {
	h := newToolsHarness(t, func() config.Tools { return config.Tools{} }, func() config.Search { return config.Search{} })
	sess := createSession(t, h)

	got := specNames(sess.tools)
	sort.Strings(got)

	want := []string{"ask_user"}
	for _, bt := range tool.Builtins(t.TempDir()) {
		want = append(want, bt.Name())
	}
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("preload-mode Specs() names = %v, want exactly %v (byte-identical to before this feature)", got, want)
	}
	if _, ok := sess.tools.Get("tool_search"); ok {
		t.Error("preload mode registered tool_search — it must not exist outside index mode")
	}
	if _, ok := sess.tools.Get("web_search"); ok {
		t.Error("preload mode registered web_search with search unconfigured")
	}
	if _, ok := sess.tools.Get(subagent.ToolName); ok {
		t.Errorf("preload mode resolved %s with subagents unconfigured — subagents are opt-in", subagent.ToolName)
	}
}

// TestSessionGuard_SpawnRegisteredOnlyWhenConfigured is
// [TestSessionGuard_WebSearchRegisteredOnlyWhenSelected]'s subagent-axis twin,
// and the direct assertion behind the standing opt-in constraint: the spawn
// tool must be absent (from Get AND from Specs) until an operator sets
// `subagents.enabled`, and present the moment they do.
func TestSessionGuard_SpawnRegisteredOnlyWhenConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Subagents
		want bool
	}{
		{"disabled (zero value)", config.Subagents{}, false},
		{"explicitly disabled", config.Subagents{Enabled: false, Agents: []string{"go-developer"}}, false},
		{"enabled", config.Subagents{Enabled: true}, true},
		{"enabled with an agent list", config.Subagents{Enabled: true, Agents: []string{"go-developer"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newToolsHarness(t, func() config.Tools { return config.Tools{} }, func() config.Search { return config.Search{} })
			h.subagents = c.cfg
			sess := createSession(t, h)
			if _, ok := sess.tools.Get(subagent.ToolName); ok != c.want {
				t.Fatalf("Get(%s) ok = %v, want %v", subagent.ToolName, ok, c.want)
			}
			if got := hasName(specNames(sess.tools), subagent.ToolName); got != c.want {
				t.Fatalf("Specs() advertises %s = %v, want %v", subagent.ToolName, got, c.want)
			}
		})
	}
}

// TestSessionGuard_WebSearchRegisteredOnlyWhenSelected covers deliverable 1's
// registration gate directly: web_search must be absent when
// config.Search.Selected() is none, and present (findable via Get, and
// advertised in Specs()) once a provider is selected — independent of
// whether the credential the tool would need at USE time actually resolves.
func TestSessionGuard_WebSearchRegisteredOnlyWhenSelected(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Search
		want bool
	}{
		{"none (zero value)", config.Search{}, false},
		{"brave selected", config.Search{Provider: "brave", Brave: config.Brave{APIKey: "env:GOFER_TEST_UNSET"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newToolsHarness(t, func() config.Tools { return config.Tools{} }, func() config.Search { return c.cfg })
			sess := createSession(t, h)
			_, ok := sess.tools.Get("web_search")
			if ok != c.want {
				t.Fatalf("Get(web_search) ok = %v, want %v", ok, c.want)
			}
			if got := hasName(specNames(sess.tools), "web_search"); got != c.want {
				t.Fatalf("Specs() advertises web_search = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSessionGuard_IndexModeFiltersSpecs proves index mode actually narrows
// what a session's FIRST model call sees: with Resident pinned to a single
// name, Specs() carries only that name plus the always-forced-resident
// tool_search — every other builtin is indexed (reachable via Get, which
// auto-promotes) but never advertised until earned.
func TestSessionGuard_IndexModeFiltersSpecs(t *testing.T) {
	h := newToolsHarness(t, func() config.Tools {
		return config.Tools{SchemaMode: "index", Resident: []string{"bash"}}
	}, func() config.Search { return config.Search{} })
	sess := createSession(t, h)

	got := specNames(sess.tools)
	sort.Strings(got)
	want := []string{"bash", "tool_search"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("index-mode Specs() names = %v, want exactly %v", got, want)
	}

	// The rest of the surface still resolves through Get (auto-promotion) —
	// index mode filters what is ADVERTISED, never what is CALLABLE.
	if _, ok := sess.tools.Get("read"); !ok {
		t.Fatal("read did not resolve through Get — index mode must not remove callable tools, only unadvertised ones")
	}
}

// TestSessionGuard_ToolSearchPromotesForNextTurn drives the real tool_search
// tool.Run (not the SDK's own toolindex unit tests — this proves gofer wired
// it into a live session's registry) and asserts the match is promoted: the
// CURRENT Specs() call right after Run already reflects it (toolindex.Index
// batches the whole match set through one Promote call — see its doc), which
// is what "resolves starting next turn" means operationally for a registry
// re-derived on every model call.
func TestSessionGuard_ToolSearchPromotesForNextTurn(t *testing.T) {
	h := newToolsHarness(t, func() config.Tools {
		return config.Tools{SchemaMode: "index"} // default resident set
	}, func() config.Search {
		return config.Search{Provider: "brave", Brave: config.Brave{APIKey: "env:GOFER_TEST_UNSET"}}
	})
	sess := createSession(t, h)

	before := specNames(sess.tools)
	if hasName(before, "web_search") {
		t.Fatalf("web_search already advertised before any search — want it indexed, not resident: %v", before)
	}

	ts, ok := sess.tools.Get("tool_search")
	if !ok {
		t.Fatal("tool_search did not resolve — it must be forced resident")
	}
	res, err := ts.Run(context.Background(), json.RawMessage(`{"query":"web_search"}`))
	if err != nil {
		t.Fatalf("tool_search.Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool_search.Run IsError, content: %s", res.Content)
	}
	if !strings.Contains(res.Content, "web_search") {
		t.Fatalf("tool_search result does not name web_search: %s", res.Content)
	}
	// The result text states the schema resolves starting NEXT turn but never
	// carries it — never the schema itself (see toolindex.SearchTool's doc).
	if strings.Contains(res.Content, `"type":"object"`) {
		t.Fatalf("tool_search result appears to carry a schema, want a summary only: %s", res.Content)
	}

	after := specNames(sess.tools)
	if !hasName(after, "web_search") {
		t.Fatalf("web_search not promoted after tool_search matched it; Specs() = %v", after)
	}
}

// TestSessionGuard_ResumeRehydratesPromotedTools proves a resumed session's
// tool array is a function of its own folded history: a tool_use block for a
// non-resident tool (web_search) in the folded context is re-promoted at
// Resume, with NO tool_search call — Specs() already carries it on the very
// first model call after resume.
func TestSessionGuard_ResumeRehydratesPromotedTools(t *testing.T) {
	root := t.TempDir()
	var resumed *fakeSession
	cfg := supervisor.Config{
		Root:  root,
		Tools: func() config.Tools { return config.Tools{SchemaMode: "index"} },
		Search: func() config.Search {
			return config.Search{Provider: "brave", Brave: config.Brave{APIKey: "env:GOFER_TEST_UNSET"}}
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			fs := newFakeSession(id, fmt.Sprintf("%s/sessions/x/%s.jsonl", root, id))
			fs.approver = opts.Approver
			fs.tools = opts.Tools
			fs.setFold([]provider.Message{
				{
					Role: provider.RoleAssistant,
					Content: []provider.ContentBlock{
						{Type: provider.BlockToolUse, ToolUseID: "1", ToolName: "web_search"},
					},
				},
				{
					Role: provider.RoleUser,
					Content: []provider.ContentBlock{
						{Type: provider.BlockToolResult, ToolUseID: "1", ToolResult: "no results"},
					},
				},
			})
			resumed = fs
			return fs, nil
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	if _, err := sup.Resume(context.Background(), "sess-resumed", supervisor.ResumeOptions{Cwd: t.TempDir(), Model: "m"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	got := specNames(resumed.tools)
	if !hasName(got, "web_search") {
		t.Fatalf("web_search not rehydrated from folded tool_use history; Specs() = %v", got)
	}
}

// TestSessionGuard_IndexSeesTheContainedDiagnosedTool covers the decorator
// order: index.Wrap MUST be outermost so its Specs() filtering covers every
// layer below it, while its Get still reaches all the way through to the
// sandboxed+lspdiag-diagnosed tool — never a plain, unwrapped one. "edit" is
// in DefaultResidentTools, so it is both advertised and resolvable without a
// tool_search round trip; its resolved %T naming lspdiag's own (unexported)
// wrapper type is the observable proof the chain is index → lspdiag →
// sandbox, not index sitting beside (or under) them.
func TestSessionGuard_IndexSeesTheContainedDiagnosedTool(t *testing.T) {
	h := newToolsHarness(t, func() config.Tools {
		return config.Tools{SchemaMode: "index"} // default resident includes "edit"
	}, func() config.Search { return config.Search{} })
	sess := createSession(t, h)

	if got := lspToolType(t, sess); !strings.Contains(got, "lspdiag") {
		t.Fatalf("edit tool type (through the index) = %s, want the lspdiag-wrapped tool", got)
	}
}
