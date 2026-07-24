package supervisor_test

import (
	"context"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestResumeRestoresJournalTitleAndCwd pins the data-fidelity fix for a
// journal-reloaded (offline) session brought back live: it must report the title
// derived from its journal's FIRST USER MESSAGE — the same label its offline row
// showed — not managed.info's project-slug fallback, and it must carry its cwd.
//
// Before the fix a resumed session had an empty title (title is only captured
// from a freshly-sent prompt in enqueue; resume restored none), so managed.info
// fell back to m.project — the cwd slug — and the roster labelled it e.g.
// "users-justin-orchestration-repos-gofer" instead of the real prompt.
func TestResumeRestoresJournalTitleAndCwd(t *testing.T) {
	h := newHarness(t)
	cwd := t.TempDir()
	ctx := context.Background()

	const prompt = "investigate the flaky build"
	id, _, _ := writeDiskJournal(t, h.root, cwd, provider.UserText(prompt))

	// The project slug is managed.info's fallback (filepath.Base of the journal's
	// project dir == session.Slugify(cwd)); the fix must NOT report it.
	slug := session.Slugify(cwd)

	info, err := h.sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: cwd, Model: "m"})
	if err != nil {
		t.Fatalf("Resume(%s): %v", id, err)
	}
	if info.Title == slug {
		t.Fatalf("resumed title fell back to the project slug %q, want the journal-derived title", slug)
	}
	if info.Title != prompt {
		t.Errorf("resumed title = %q, want journal-derived %q", info.Title, prompt)
	}
	if info.Cwd != cwd {
		t.Errorf("resumed cwd = %q, want persisted %q", info.Cwd, cwd)
	}

	// The seed lives on the managed session, not just this return value: a later
	// roster read reports the same title.
	roster, err := h.sup.OverviewRoster(ctx)
	if err != nil {
		t.Fatalf("OverviewRoster: %v", err)
	}
	got := findInfo(roster, id)
	if got == nil {
		t.Fatalf("resumed session %s absent from the overview: %+v", id, roster)
	}
	if got.Title != prompt {
		t.Errorf("overview title for %s = %q, want %q", id, got.Title, prompt)
	}
}

// TestResumeTitleSeedIgnoresNoUserMessage keeps the fallback intact: a journal
// with no user message (nothing to derive a title from) must still resume, and
// managed.info's project-slug fallback stays in force rather than the seed
// forcing an empty title.
func TestResumeTitleSeedIgnoresNoUserMessage(t *testing.T) {
	h := newHarness(t)
	cwd := t.TempDir()
	ctx := context.Background()

	// A journal with only its meta entry — no user message.
	id, _, _ := writeDiskJournal(t, h.root, cwd)

	info, err := h.sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: cwd, Model: "m"})
	if err != nil {
		t.Fatalf("Resume(%s): %v", id, err)
	}
	if info.Title != session.Slugify(cwd) {
		t.Errorf("titleless resume = %q, want project-slug fallback %q", info.Title, session.Slugify(cwd))
	}
}
