package supervisor_test

// spawntool_test.go covers the AGENT-INITIATED half of subagents: the
// `spawn_subagent` tool actually creating a child session through the seam, the
// depth cap refusing one as a non-fatal tool result rather than a killed turn,
// and the SDK journal agreeing with gofer's sidecar about who a child's parent
// is.
//
// Every test here drives the REAL tool out of the registry the supervisor built
// (never the Spawner seam directly), so the schema, the bind-after-mint id, the
// registration gate, and the supervisor's own refusals are all in the path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/subagent"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// runSpawnTool resolves the spawn tool out of sess's own registry — the one the
// supervisor built and bound — and runs it with the given JSON input, exactly
// as the SDK loop would on a model tool call.
func runSpawnTool(t *testing.T, sess *fakeSession, input string) loop.ToolResult {
	t.Helper()
	tl, ok := sess.tools.Get(subagent.ToolName)
	if !ok {
		t.Fatalf("%s is not registered on this session", subagent.ToolName)
	}
	res, err := tl.Run(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s.Run returned a Go error (it must only do that for undecodable input or a cancelled ctx): %v", subagent.ToolName, err)
	}
	return res
}

// TestSpawnToolCreatesLinkedChild is the agent-initiated happy path: the model
// calls the tool, and a REAL child session exists — linked to the caller at
// depth 1, carrying the requested agent id, inheriting the parent's model and
// cwd, and with the durable sidecar on disk that makes the link survive a
// restart.
func TestSpawnToolCreatesLinkedChild(t *testing.T) {
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.SubagentsConfig = func() config.Subagents {
			return config.Subagents{Enabled: true, Agents: []string{"go-developer", "go-reviewer"}}
		}
	})
	ctx := context.Background()

	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	ps := h.session(parent.ID)

	res := runSpawnTool(t, ps, `{"agent":"go-developer","prompt":"investigate the flaky build"}`)
	if res.IsError {
		t.Fatalf("spawn returned an error result: %s", res.Content)
	}

	roster, err := h.sup.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	var child *supervisor.SessionInfo
	for i := range roster {
		if roster[i].ParentID == parent.ID {
			child = &roster[i]
		}
	}
	if child == nil {
		t.Fatalf("no child session was created; roster = %+v", roster)
	}
	if !strings.Contains(res.Content, child.ID) {
		t.Errorf("tool result %q does not name the child session id %s", res.Content, child.ID)
	}
	if child.Agent != "go-developer" {
		t.Errorf("child Agent = %q, want go-developer", child.Agent)
	}
	if child.Depth != 1 {
		t.Errorf("child Depth = %d, want 1", child.Depth)
	}
	// Inheritance, not configuration — see localSubagents.Spawn's doc.
	if child.Model != "claude-haiku-4-5" || child.Cwd != "/proj" {
		t.Errorf("child = {model %q, cwd %q}, want the parent's {claude-haiku-4-5, /proj}", child.Model, child.Cwd)
	}
	// The child was seeded with the brief as its first turn, not left idle.
	if got := h.session(child.ID).waitStarted(t); got != "investigate the flaky build" {
		t.Errorf("child's first turn = %q, want the brief the model wrote", got)
	}
	// And the link is durable, not roster-only.
	sidecar := filepath.Join(h.root, "sessions", session.Slugify("/proj"), child.ID+".meta.json")
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("no sidecar for the agent-spawned child: %v", err)
	}
}

// TestSpawnToolAtMaxDepthIsRefusedNonFatally is the cap on the AGENT path,
// driven through the tool rather than through Create: a model that asks for one
// child too many must be told so and keep working. A Go error here would abort
// the parent's whole turn over a recoverable planning mistake.
func TestSpawnToolAtMaxDepthIsRefusedNonFatally(t *testing.T) {
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.MaxSubagentDepth = 1
		cfg.SubagentsConfig = func() config.Subagents { return config.Subagents{Enabled: true} }
	})
	ctx := context.Background()

	root, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: "/proj", Model: "claude-haiku-4-5", ParentID: root.ID,
	})
	if err != nil {
		t.Fatalf("Create child at the cap: %v", err)
	}

	before, err := h.sup.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}

	// The child sits AT the cap, so its own spawn would be depth 2.
	res := runSpawnTool(t, h.session(child.ID), `{"prompt":"go deeper"}`)
	if !res.IsError {
		t.Fatalf("spawn past the cap succeeded: %s", res.Content)
	}
	if !strings.Contains(res.Content, "max_subagent_depth") {
		t.Errorf("refusal %q does not name the config key that raises the cap", res.Content)
	}

	after, err := h.sup.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused spawn created a session anyway: %d rows before, %d after", len(before), len(after))
	}
}

// TestSpawnToolRejectsUnknownAgent pins the configured-agent gate: when an
// operator names the agents, one the model invented is refused as a
// model-correctable result naming the valid set — not silently accepted and not
// a failed turn.
func TestSpawnToolRejectsUnknownAgent(t *testing.T) {
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.SubagentsConfig = func() config.Subagents {
			return config.Subagents{Enabled: true, Agents: []string{"go-developer"}}
		}
	})
	ctx := context.Background()

	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := runSpawnTool(t, h.session(parent.ID), `{"agent":"rust-developer","prompt":"do a thing"}`)
	if !res.IsError {
		t.Fatalf("unknown agent accepted: %s", res.Content)
	}
	if !strings.Contains(res.Content, "go-developer") {
		t.Errorf("refusal %q does not name the configured agents", res.Content)
	}
	if roster, err := h.sup.Roster(ctx); err != nil {
		t.Fatalf("Roster: %v", err)
	} else if len(roster) != 1 {
		t.Errorf("a refused spawn created a session anyway: %+v", roster)
	}
}

// TestSpawnToolBindsItsOwnSessionID is the bind-after-mint assertion. The tool
// is built in sessionGuard, BEFORE runner.New mints the id that becomes the
// ParentID of everything it spawns, so a missing Bind would either spawn roots
// (dodging the depth cap entirely) or refuse. Two sibling sessions each spawn a
// child, and each child must be attached to the session that asked — which a
// single-session test could not distinguish from a tool that captured the wrong
// id once.
func TestSpawnToolBindsItsOwnSessionID(t *testing.T) {
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.SubagentsConfig = func() config.Subagents { return config.Subagents{Enabled: true} }
	})
	ctx := context.Background()

	var parents []string
	for range 2 {
		info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		parents = append(parents, info.ID)
	}
	for _, id := range parents {
		if res := runSpawnTool(t, h.session(id), fmt.Sprintf(`{"prompt":"child of %s"}`, id)); res.IsError {
			t.Fatalf("spawn from %s: %s", id, res.Content)
		}
	}

	roster, err := h.sup.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	children := map[string]int{}
	for _, row := range roster {
		if row.ParentID != "" {
			children[row.ParentID]++
		}
	}
	for _, id := range parents {
		if children[id] != 1 {
			t.Errorf("session %s has %d children, want exactly 1 — the tool bound the wrong session id; roster = %+v",
				id, children[id], roster)
		}
	}
}

// TestSpawnToolAbsentWhenDisabled is the negative of the whole feature at the
// call site a model would actually use: with subagents unconfigured, the tool
// does not resolve out of the registry at all.
func TestSpawnToolAbsentWhenDisabled(t *testing.T) {
	h := newHarness(t) // no SubagentsConfig: the zero value, disabled
	parent, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := h.session(parent.ID).tools.Get(subagent.ToolName); ok {
		t.Fatalf("%s resolved with subagents disabled", subagent.ToolName)
	}
}

// TestNoSeamMeansNoSpawning pins the OTHER switch, and it is a regression test
// for a real hole: a supervisor built with no [supervisor.Config.Subagents]
// factory has no way to create a child session, and config alone must not be
// able to conjure one.
//
// The hole (gofer#345 review): Config.Subagents used to treat nil as "install
// the in-process localSubagents", so the seam failed OPEN. A `gofer
// session-worker` started WITHOUT `--router` supplies no seam — it cannot, since
// its embedded daemon is MaxSessions: 1 and a worker never creates a session —
// and therefore silently received the session-creating implementation. With
// `subagents.enabled` set in config, spawn_subagent then minted children inside
// a single-session worker, bypassing the router and the daemon's MaxSessions
// cap both.
//
// The subagents CONFIG here is enabled on purpose: this test is meaningless
// against the disabled zero value, because then the config switch alone would
// explain the absence and the seam's polarity would go untested.
func TestNoSeamMeansNoSpawning(t *testing.T) {
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		// The deployment a worker with no router dial-back is in: enabled by
		// config, and structurally unable to host a child.
		cfg.Subagents = nil
		cfg.SubagentsConfig = func() config.Subagents { return config.Subagents{Enabled: true} }
	})
	ctx := context.Background()

	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	if _, ok := h.session(parent.ID).tools.Get(subagent.ToolName); ok {
		t.Fatalf("%s resolved on a supervisor with no subagent seam — enabling it in config must not install a local spawner", subagent.ToolName)
	}

	// The report half rides the same seam and must be off for the same reason:
	// a child here has nothing to report THROUGH. Registering it would mean the
	// supervisor found a spawner after all.
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: "/proj", Model: "claude-haiku-4-5", ParentID: parent.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	cs := h.session(child.ID)
	cs.setFold([]provider.Message{provider.AssistantText("done")})
	if err := h.sup.Send(ctx, child.ID, "do the work"); err != nil {
		t.Fatalf("Send child: %v", err)
	}
	cs.waitStarted(t)
	cs.finish(t, nil)
	waitForStatus(t, h.sup, child.ID, supervisor.StatusNeedsInput)

	// A report reaches the parent as its next prompt (see
	// managed.reportToParentOnce), so it is visible either still queued or
	// already run. Both must be empty.
	if q, err := h.sup.QueueList(ctx, parent.ID); err != nil {
		t.Fatalf("QueueList parent: %v", err)
	} else if len(q) != 0 {
		t.Fatalf("child queued a report on its parent with no subagent seam installed: %v", q)
	}
	if got := reportPrompts(h.session(parent.ID)); len(got) != 0 {
		t.Fatalf("parent ran a report turn with no subagent seam installed: %v", got)
	}
}

// TestSubagentJournalMetaAgreesWithSidecar is the D1 agreement test, and it
// exists because the premise this feature was scoped against went stale.
//
// The SDK journal DOES now carry a session's parentage: agent-sdk-go v0.23.0
// writes parent_id/depth into the root session_meta entry when runner.Options
// names them (session.WithMetaParent). gofer forwards them — that is what
// "consuming the Runner.Spawn seam" means here, since Spawn itself cannot be
// called across the factory seam or a worker process boundary — so the two
// records now coexist and MUST agree.
//
// gofer's sidecar remains the AUTHORITY: it is what resolveParent derives depth
// from and what the roster reads. The journal entry is a write-only projection
// with no reader anywhere in gofer. This test pins that they say the same thing,
// so the projection can never quietly drift into a second, contradictory truth.
//
// It runs against a REAL *runner.Runner over the faux provider, because a fake
// session writes no journal and could not observe the SDK half at all.
func TestSubagentJournalMetaAgreesWithSidecar(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sup, err := supervisor.New(supervisor.Config{
		Root:             root,
		Store:            store,
		MaxSubagentDepth: 3,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Provider = faux.New(faux.Default())
			return runner.New(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	ctx := context.Background()
	parent, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: cwd, Model: "faux-1"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: cwd, Model: "faux-1", ParentID: parent.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	// The SDK's projection.
	entries, err := session.ReadEntries(child.JournalPath)
	if err != nil {
		t.Fatalf("ReadEntries(child): %v", err)
	}
	meta, ok := session.MetaOf(entries)
	if !ok {
		t.Fatalf("child journal has no session_meta entry")
	}
	// gofer's authority.
	sidecarParent, sidecarAgent, sidecarDepth := supervisor.DiskMeta(root, child.ID)
	if sidecarParent == "" {
		t.Fatalf("child has no sidecar parent link")
	}
	if meta.ParentID != sidecarParent {
		t.Errorf("journal parent_id = %q, sidecar parentId = %q — the projection disagrees with the authority",
			meta.ParentID, sidecarParent)
	}
	if meta.Depth != sidecarDepth {
		t.Errorf("journal depth = %d, sidecar depth = %d — the projection disagrees with the authority",
			meta.Depth, sidecarDepth)
	}
	// The agent id is gofer-native and has no journal counterpart at all, which
	// is exactly why the sidecar cannot be replaced by the SDK's entry.
	if sidecarAgent != "go-developer" {
		t.Errorf("sidecar agent = %q, want go-developer", sidecarAgent)
	}
	if meta.ParentID != parent.ID || meta.Depth != 1 {
		t.Errorf("journal meta = {parent %q, depth %d}, want {%q, 1}", meta.ParentID, meta.Depth, parent.ID)
	}

	// A ROOT session's journal must be untouched by this forwarding: gofer
	// passes zero values, runner.New passes no WithMetaParent at all, and the
	// meta payload's fields are omitempty — so an unchanged root session's
	// journal stays byte-identical to one written before subagents existed.
	rootEntries, err := session.ReadEntries(parent.JournalPath)
	if err != nil {
		t.Fatalf("ReadEntries(parent): %v", err)
	}
	rootMeta, ok := session.MetaOf(rootEntries)
	if !ok {
		t.Fatalf("parent journal has no session_meta entry")
	}
	if rootMeta.ParentID != "" || rootMeta.Depth != 0 {
		t.Errorf("root journal meta = {parent %q, depth %d}, want both zero", rootMeta.ParentID, rootMeta.Depth)
	}
	raw, err := json.Marshal(rootMeta)
	if err != nil {
		t.Fatalf("marshal root meta: %v", err)
	}
	for _, key := range []string{"parent_id", "depth"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("root session_meta payload carries %q (%s) — it must stay byte-identical to a pre-subagent journal", key, raw)
		}
	}
}

// TestCreateForwardsDepthPolicyToRunnerOptions reads the options the session
// factory was actually handed, which is the only place the forwarding is
// observable for a supervisor whose sessions are fakes. Asserting the roster row
// instead would pass even if the SDK were never told anything.
func TestCreateForwardsDepthPolicyToRunnerOptions(t *testing.T) {
	cwd := t.TempDir()
	h := newSpawnHarness(t, t.TempDir(), 3)
	ctx := context.Background()

	root, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Model: "faux", Cwd: cwd})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Model: "faux", Cwd: cwd, ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	rootOpts := h.optsFor(root.ID)
	if rootOpts.ParentID != "" || rootOpts.Depth != 0 {
		t.Errorf("root runner.Options = {parent %q, depth %d}, want both zero", rootOpts.ParentID, rootOpts.Depth)
	}
	if rootOpts.MaxDepth != 3 {
		t.Errorf("root runner.Options.MaxDepth = %d, want the supervisor's resolved cap 3", rootOpts.MaxDepth)
	}
	childOpts := h.optsFor(child.ID)
	if childOpts.ParentID != root.ID || childOpts.Depth != 1 || childOpts.MaxDepth != 3 {
		t.Errorf("child runner.Options = {parent %q, depth %d, maxDepth %d}, want {%q, 1, 3}",
			childOpts.ParentID, childOpts.Depth, childOpts.MaxDepth, root.ID)
	}
}

// TestSpawnToolSurfacesSupervisorRefusalNonFatally covers the remaining refusal
// class the seam can return — here a CLOSED supervisor — as one more non-fatal
// result. The tool must never let a supervisor-level error escape as a Go error
// that aborts the parent's turn.
func TestSpawnToolSurfacesSupervisorRefusalNonFatally(t *testing.T) {
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.SubagentsConfig = func() config.Subagents { return config.Subagents{Enabled: true} }
	})
	ctx := context.Background()

	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ps := h.session(parent.ID)
	if err := h.sup.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := runSpawnTool(t, ps, `{"prompt":"too late"}`)
	if !res.IsError {
		t.Fatalf("spawn on a closed supervisor succeeded: %s", res.Content)
	}
	if !strings.Contains(res.Content, subagent.ToolName) {
		t.Errorf("refusal %q does not name the tool", res.Content)
	}
	// Sanity: the underlying refusal really is the supervisor's, not a
	// validation rejection that would have fired regardless.
	if _, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"}); !errors.Is(err, supervisor.ErrClosed) {
		t.Fatalf("Create after Close = %v, want ErrClosed", err)
	}
}
