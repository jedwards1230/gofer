package supervisor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// subagent_test.go covers the parent/child session primitive: the parent link
// itself, the config-driven depth cap, the durable `<id>.meta.json` sidecar, and
// the runner.Options.Agent forwarding a child's tool-call attribution rides on.

// spawnHarness is a Supervisor whose session factory returns scripted
// [fakeSession]s (see helpers_test.go) AND touches a real `<id>.jsonl` under the
// store root. The journal file is what makes these tests exercise the REAL disk
// paths: [supervisor.Supervisor.List] enumerates the store's own *.jsonl files,
// and the parent lookup keys existence off the same file — a pure in-memory fake
// would silently skip both.
//
// It also records the [runner.Options] each factory call received, which is how
// a test asserts the Agent id actually reaches the SDK seam rather than merely
// showing up in the roster row.
type spawnHarness struct {
	t    *testing.T
	sup  *supervisor.Supervisor
	root string

	mu   sync.Mutex
	n    int
	opts map[string]runner.Options
}

// newSpawnHarness builds a Supervisor over root with the given subagent depth
// cap (0 = unset, i.e. the config default).
func newSpawnHarness(t *testing.T, root string, maxDepth int) *spawnHarness {
	t.Helper()
	h := &spawnHarness{t: t, root: root, opts: make(map[string]runner.Options)}

	sup, err := supervisor.New(supervisor.Config{
		Root:             root,
		MaxSubagentDepth: maxDepth,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			return h.build(fmt.Sprintf("sess-%d", h.next()), opts)
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return h.build(id, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	h.sup = sup
	t.Cleanup(func() { _ = sup.Close() })
	return h
}

func (h *spawnHarness) next() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	return h.n
}

// build records opts for id and returns a fake session backed by a real (empty)
// journal file at the path a FileStore would have used.
func (h *spawnHarness) build(id string, opts runner.Options) (supervisor.Session, error) {
	h.t.Helper()
	dir := filepath.Join(h.root, "sessions", session.Slugify(opts.Cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.opts[id] = opts
	h.mu.Unlock()
	return newFakeSession(id, path), nil
}

// optsFor returns the runner.Options the factory built id with.
func (h *spawnHarness) optsFor(id string) runner.Options {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.opts[id]
}

// TestCreateSubagentLinksParent pins the primitive itself: a create naming a
// parent produces a real child session carrying ParentID/Agent/Depth, and depth
// increments across two levels. A create naming NO parent is a root session at
// depth 0 — the shape every session gofer created before subagents existed has.
func TestCreateSubagentLinksParent(t *testing.T) {
	cwd := t.TempDir()
	h := newSpawnHarness(t, t.TempDir(), 0)
	ctx := context.Background()

	root, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Model: "faux", Cwd: cwd})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Model: "faux", Cwd: cwd, ParentID: root.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	grandchild, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Model: "faux", Cwd: cwd, ParentID: child.ID, Agent: "go-reviewer",
	})
	if err != nil {
		t.Fatalf("Create grandchild: %v", err)
	}

	tests := []struct {
		name       string
		got        supervisor.SessionInfo
		wantParent string
		wantAgent  string
		wantDepth  int
	}{
		{"root session has no parent and depth 0", root, "", "", 0},
		{"child links its parent at depth 1", child, root.ID, "go-developer", 1},
		{"grandchild nests one deeper", grandchild, child.ID, "go-reviewer", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.ParentID != tc.wantParent {
				t.Errorf("ParentID = %q, want %q", tc.got.ParentID, tc.wantParent)
			}
			if tc.got.Agent != tc.wantAgent {
				t.Errorf("Agent = %q, want %q", tc.got.Agent, tc.wantAgent)
			}
			if tc.got.Depth != tc.wantDepth {
				t.Errorf("Depth = %d, want %d", tc.got.Depth, tc.wantDepth)
			}
			// The live roster must report the same link the create returned —
			// otherwise a client polling the roster would lose the tree.
			live, err := h.sup.Roster(context.Background())
			if err != nil {
				t.Fatalf("Roster: %v", err)
			}
			row := findInfo(live, tc.got.ID)
			if row == nil {
				t.Fatalf("Roster missing %s", tc.got.ID)
			}
			if row.ParentID != tc.wantParent || row.Agent != tc.wantAgent || row.Depth != tc.wantDepth {
				t.Errorf("roster row = {parent %q, agent %q, depth %d}, want {%q, %q, %d}",
					row.ParentID, row.Agent, row.Depth, tc.wantParent, tc.wantAgent, tc.wantDepth)
			}
		})
	}
}

// TestCreateForwardsAgentToRunnerOptions is the SDK-seam assertion: the Agent id
// must reach [runner.Options.Agent], which is what makes the session's loop stamp
// it onto every tool-call event. Asserting only the roster row would pass even if
// the option were never forwarded, so this reads the options the session factory
// was actually handed.
func TestCreateForwardsAgentToRunnerOptions(t *testing.T) {
	tests := []struct {
		name  string
		agent string
	}{
		{"no agent stays un-attributed", ""},
		{"agent id is forwarded verbatim", "go-developer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newSpawnHarness(t, t.TempDir(), 0)
			info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{
				Model: "faux", Cwd: t.TempDir(), Agent: tc.agent,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := h.optsFor(info.ID).Agent; got != tc.agent {
				t.Errorf("runner.Options.Agent = %q, want %q", got, tc.agent)
			}
		})
	}
}

// TestCreateSubagentDepthCap walks a chain down to the cap and one step past it.
// The cap is read from config, so a non-default value must actually change where
// the refusal lands — a test pinned only to the default would pass against a
// hardcoded literal.
func TestCreateSubagentDepthCap(t *testing.T) {
	tests := []struct {
		name     string
		cap      int
		wantDeep int // the deepest depth that must still be creatable
	}{
		{"cap of 1 allows one level of children", 1, 1},
		{"a non-default cap of 3 allows three", 3, 3},
		{"unset falls back to the package default", 0, config.DefaultMaxSubagentDepth},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			h := newSpawnHarness(t, t.TempDir(), tc.cap)
			ctx := context.Background()

			cur, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Model: "faux", Cwd: cwd})
			if err != nil {
				t.Fatalf("Create root: %v", err)
			}
			for depth := 1; depth <= tc.wantDeep; depth++ {
				cur, err = h.sup.Create(ctx, "", supervisor.CreateOptions{
					Model: "faux", Cwd: cwd, ParentID: cur.ID,
				})
				if err != nil {
					t.Fatalf("Create at depth %d: %v (want it accepted, cap %d)", depth, err, tc.wantDeep)
				}
				if cur.Depth != depth {
					t.Fatalf("Depth = %d, want %d", cur.Depth, depth)
				}
			}

			// One past the cap must be refused, with the sentinel a caller branches on.
			_, err = h.sup.Create(ctx, "", supervisor.CreateOptions{
				Model: "faux", Cwd: cwd, ParentID: cur.ID,
			})
			if !errors.Is(err, supervisor.ErrDepthExceeded) {
				t.Fatalf("Create at depth %d = %v, want ErrDepthExceeded", tc.wantDeep+1, err)
			}
			// The message must name the remedy, not just the failure.
			if msg := err.Error(); !strings.Contains(msg, "max_subagent_depth") {
				t.Errorf("error %q does not name the config key that raises the cap", msg)
			}
		})
	}
}

// TestCreateUnknownParentRejected covers the other refusal: an id that names no
// session at all, live or on disk. Linking to it would produce a row no roster
// could ever place in a tree, so the create fails rather than silently demoting
// the session to a root.
func TestCreateUnknownParentRejected(t *testing.T) {
	h := newSpawnHarness(t, t.TempDir(), 0)
	_, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{
		Model: "faux", Cwd: t.TempDir(), ParentID: "no-such-session",
	})
	if !errors.Is(err, supervisor.ErrNoParent) {
		t.Fatalf("Create = %v, want ErrNoParent", err)
	}
}

// TestSubagentLinkSurvivesRestart is the durability assertion: the link is an
// on-disk artifact next to the journal, not roster-only memory. A SECOND
// supervisor over the same root — the shape a daemon restart (or another M6
// worker over the shared store) sees — must still report it from List.
func TestSubagentLinkSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	ctx := context.Background()

	h := newSpawnHarness(t, root, 0)
	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Model: "faux", Cwd: cwd})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Model: "faux", Cwd: cwd, ParentID: parent.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := h.sup.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The sidecar is a visible artifact, so assert its bytes, not just the
	// behavior they produce.
	sidecar := filepath.Join(root, "sessions", session.Slugify(cwd), child.ID+".meta.json")
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var got struct {
		ParentID string `json:"parentId"`
		Agent    string `json:"agent"`
		Depth    int    `json:"depth"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal sidecar %s: %v", raw, err)
	}
	if got.ParentID != parent.ID || got.Agent != "go-developer" || got.Depth != 1 {
		t.Errorf("sidecar = %+v, want {parent %s, agent go-developer, depth 1}", got, parent.ID)
	}
	// A root session records nothing at all — this feature is invisible to a
	// session that has no parent and no agent.
	if _, err := os.Stat(filepath.Join(root, "sessions", session.Slugify(cwd), parent.ID+".meta.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("root session wrote a sidecar (stat err = %v), want none", err)
	}

	restarted := newSpawnHarness(t, root, 0)
	infos, err := restarted.sup.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	row := findInfo(infos, child.ID)
	if row == nil {
		t.Fatalf("List missing child %s: %+v", child.ID, infos)
	}
	if row.ParentID != parent.ID || row.Agent != "go-developer" || row.Depth != 1 {
		t.Errorf("restarted List row = {parent %q, agent %q, depth %d}, want {%q, go-developer, 1}",
			row.ParentID, row.Agent, row.Depth, parent.ID)
	}

	// And an offline parent still resolves, so a child can be spawned under a
	// session this process never created.
	deeper, err := restarted.sup.Create(ctx, "", supervisor.CreateOptions{
		Model: "faux", Cwd: cwd, ParentID: child.ID,
	})
	if err != nil {
		t.Fatalf("Create under an offline parent: %v", err)
	}
	if deeper.Depth != 2 {
		t.Errorf("Depth = %d, want 2 (offline parent's persisted depth + 1)", deeper.Depth)
	}
}

// TestResumeRestoresAgentAttribution pins that a resumed child keeps its
// attribution: Agent must be read back from the sidecar and handed to the SDK
// again, or the resumed session's tool calls would silently lose their agent id.
func TestResumeRestoresAgentAttribution(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	ctx := context.Background()

	h := newSpawnHarness(t, root, 0)
	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Model: "faux", Cwd: cwd})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Model: "faux", Cwd: cwd, ParentID: parent.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := h.sup.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted := newSpawnHarness(t, root, 0)
	info, err := restarted.sup.Resume(ctx, child.ID, supervisor.ResumeOptions{Model: "faux", Cwd: cwd})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if info.ParentID != parent.ID || info.Agent != "go-developer" || info.Depth != 1 {
		t.Errorf("resumed row = {parent %q, agent %q, depth %d}, want {%q, go-developer, 1}",
			info.ParentID, info.Agent, info.Depth, parent.ID)
	}
	if got := restarted.optsFor(child.ID).Agent; got != "go-developer" {
		t.Errorf("resumed runner.Options.Agent = %q, want go-developer", got)
	}
}

// TestListWithoutSidecarUnchanged is the backward-compatibility assertion: every
// session already on disk has no sidecar, and must keep listing exactly as it
// did — no error, zero-value subagent fields, journal enrichment intact.
func TestListWithoutSidecarUnchanged(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id, _, _ := writeDiskJournal(t, root, cwd, provider.UserText("investigate the flaky build"))

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing sidecar-less session %s: %+v", id, infos)
	}
	if got.ParentID != "" || got.Agent != "" || got.Depth != 0 {
		t.Errorf("sidecar-less row = {parent %q, agent %q, depth %d}, want all zero",
			got.ParentID, got.Agent, got.Depth)
	}
	if got.Cwd != cwd || got.Title != "investigate the flaky build" {
		t.Errorf("journal enrichment regressed: cwd %q, title %q", got.Cwd, got.Title)
	}
}

// TestListCorruptSidecarDegradesGracefully pins the "enriches, never fails"
// rule: a sidecar that is not valid JSON must read back as zero values, leaving
// the session listable exactly as a sidecar-less one. A parse error here would
// let one bad file hide a session from every roster.
func TestListCorruptSidecarDegradesGracefully(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id, slug, _ := writeDiskJournal(t, root, cwd, provider.UserText("hi"))
	if err := os.WriteFile(filepath.Join(root, "sessions", slug, id+".meta.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findInfo(infos, id)
	if got == nil {
		t.Fatalf("List missing session %s with a corrupt sidecar: %+v", id, infos)
	}
	if got.ParentID != "" || got.Agent != "" || got.Depth != 0 {
		t.Errorf("corrupt-sidecar row = {parent %q, agent %q, depth %d}, want all zero",
			got.ParentID, got.Agent, got.Depth)
	}
	if got.Title != "hi" {
		t.Errorf("Title = %q, want hi (journal enrichment must be unaffected)", got.Title)
	}
}

// TestSidecarIsNotMistakenForASession pins the file-naming choice: the store
// walk List drives selects on the ".jsonl" suffix, so a `<id>.meta.json` living
// beside `<id>.jsonl` must not double-count as a second session. Getting this
// wrong would show every subagent twice in the roster.
func TestSidecarIsNotMistakenForASession(t *testing.T) {
	root := t.TempDir()
	const slug = "sidecar-proj"
	dir := filepath.Join(root, "sessions", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const id = "x"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), nil, 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".meta.json"), []byte(`{"parentId":"p","agent":"a","depth":1}`), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List returned %d sessions, want exactly 1: %+v", len(infos), infos)
	}
	if infos[0].ID != id {
		t.Errorf("session id = %q, want %q", infos[0].ID, id)
	}
	if infos[0].ParentID != "p" || infos[0].Agent != "a" || infos[0].Depth != 1 {
		t.Errorf("row = {parent %q, agent %q, depth %d}, want {p, a, 1} from the sidecar",
			infos[0].ParentID, infos[0].Agent, infos[0].Depth)
	}
}
