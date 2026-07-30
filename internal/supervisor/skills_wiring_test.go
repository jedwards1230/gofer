package supervisor_test

// skills_wiring_test.go proves config.Skills actually reaches sessionGuard's
// tool registration and its diagnostic surfacing — not just that
// internal/skillset behaves correctly in isolation (that is its own
// skillset_test.go), and not just that the precedence fix holds at the
// config layer (config/skills_test.go). A test that only checked those two
// would keep passing even if sessionGuard never called skillset.Load at all.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// writeSkillFixture writes <dir>/<name>/SKILL.md.
func writeSkillFixture(t *testing.T, dir, name, description, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

// newHarnessWithConfig is [newHarness] (helpers_test.go) with an explicit
// Skills resolver injected — newHarness itself builds a Config with none
// set, so it cannot exercise this knob. Mirrors newLSPHarness's shape
// (lspdiag_config_test.go) for the same reason.
func newHarnessWithConfig(t *testing.T, skills func() config.Skills) *harness {
	t.Helper()
	h := &harness{t: t, root: t.TempDir(), sessions: make(map[string]*fakeSession)}

	var nextID int64
	cfg := supervisor.Config{
		Root:   h.root,
		Skills: skills,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := fmt.Sprintf("sess-%d", atomic.AddInt64(&nextID, 1))
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

// TestSessionGuard_SkillsConfigReachesRegistry proves a skill discovered
// under an explicit config.Skills.Dirs is registered as the "skill" tool a
// real session's registry resolves — the end-to-end pipe from config to
// sessionGuard's extra tools.
func TestSessionGuard_SkillsConfigReachesRegistry(t *testing.T) {
	skillsDir := t.TempDir()
	writeSkillFixture(t, skillsDir, "review", "reviews a diff", "do the review")

	h := newHarnessWithConfig(t, func() config.Skills { return config.Skills{Dirs: []string{skillsDir}} })

	info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess := h.session(info.ID)
	if sess.tools == nil {
		t.Fatal("supervisor did not inject a tool registry")
	}
	if _, ok := sess.tools.Get("skill"); !ok {
		t.Fatal(`Get("skill") not found — config.Skills.Dirs did not reach sessionGuard`)
	}
}

// TestSessionGuard_NoSkillsOmitsTheTool proves the context-cost side of the
// same wiring: with nothing configured (no <root>/skills, no
// <cwd>/.gofer/skills present on disk), the "skill" tool is NOT registered
// at all — a permanently-empty tool is pure context cost with no payoff.
func TestSessionGuard_NoSkillsOmitsTheTool(t *testing.T) {
	h := newHarnessWithConfig(t, nil)

	info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess := h.session(info.ID)
	if _, ok := sess.tools.Get("skill"); ok {
		t.Fatal(`Get("skill") found, want absent — no skills are configured`)
	}
}

// TestSessionGuard_SkillDiagnosticIsVisible proves a skipped SKILL.md
// reaches the session's own event stream as a non-fatal session.error — the
// "visible artifact, never swallowed" treatment — rather than being dropped
// on the floor inside sessionGuard.
func TestSessionGuard_SkillDiagnosticIsVisible(t *testing.T) {
	skillsDir := t.TempDir()
	// A body well past a tiny cap is skipped with a Diagnostic (see
	// internal/skillset.Load / the SDK's skill.Load).
	writeSkillFixture(t, skillsDir, "huge", "a skill with a huge body", string(make([]byte, 1000)))

	tiny := 10
	h := newHarnessWithConfig(t, func() config.Skills {
		return config.Skills{Dirs: []string{skillsDir}, MaxFileBytes: &tiny}
	})

	info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess := h.session(info.ID)

	sub := sess.Events()
	defer sub.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-sub.C:
			se, ok := e.(event.SessionError)
			if !ok {
				continue
			}
			if se.Err == "" {
				t.Fatal("session.error carries no message")
			}
			if se.Fatal {
				t.Fatal("session.error Fatal = true, want false — a skipped skill must not end the session")
			}
			return // found it
		case <-deadline:
			t.Fatal("timed out waiting for the skipped skill's session.error")
		}
	}
}
