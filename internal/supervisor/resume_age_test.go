package supervisor_test

import (
	"context"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestResumeKeepsJournalLastActivityTime pins the overview-age fix: resuming a
// session off disk must NOT advance its last-activity time to the moment it was
// reopened. The overview's age column is rendered from SessionInfo.Updated
// (humanAge(now - Updated)), so a resume that stamped Updated to "now" made every
// reopened row read "now" regardless of when its last real conversation event
// was. A resumed session must instead keep the journal's last-entry time — the
// exact value its offline row showed (diskSessionInfo.Updated) — so viewing a
// session is not mistaken for working on it.
func TestResumeKeepsJournalLastActivityTime(t *testing.T) {
	h := newHarness(t)
	cwd := t.TempDir()
	ctx := context.Background()

	id, _, _ := writeDiskJournal(t, h.root, cwd, provider.UserText("investigate the flaky build"))

	// The offline row's Updated is the journal's last entry time (see
	// diskSessionInfo) — the value the roster showed before the resume, and the
	// value the resumed live row must preserve.
	offline, err := h.sup.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	before := findInfo(offline, id)
	if before == nil {
		t.Fatalf("List missing disk-only session %s: %+v", id, offline)
	}
	if before.Updated.IsZero() {
		t.Fatalf("precondition: offline Updated is zero, want the journal's last entry time")
	}

	info, err := h.sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: cwd, Model: "m"})
	if err != nil {
		t.Fatalf("Resume(%s): %v", id, err)
	}
	// The resume return value already carries the seeded time.
	if !info.Updated.Equal(before.Updated) {
		t.Errorf("resumed Updated = %v, want the journal's last-activity time %v (resume must not restamp it to now)", info.Updated, before.Updated)
	}

	// The seed lives on the managed session, not just this return value: a later
	// roster read reports the same last-activity time.
	got := findInfo(roster(t, h.sup), id)
	if got == nil {
		t.Fatalf("resumed session %s absent from the roster", id)
	}
	if !got.Updated.Equal(before.Updated) {
		t.Errorf("roster Updated for resumed %s = %v, want journal time %v", id, got.Updated, before.Updated)
	}
}

// TestResumeThenPromptAdvancesLastActivity is the other half of the fix: a
// resumed session that actually does new work SHOULD advance its last-activity
// time. The seed only holds while the session is untouched (managed.resumed) —
// the first prompt hands Updated back to the pump, which bumps it on every turn
// transition. So the age reflects real activity, and only lifecycle actions
// (resume/attach/view) leave it alone.
func TestResumeThenPromptAdvancesLastActivity(t *testing.T) {
	h := newHarness(t)
	cwd := t.TempDir()
	ctx := context.Background()

	id, _, _ := writeDiskJournal(t, h.root, cwd, provider.UserText("investigate the flaky build"))

	info, err := h.sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: cwd, Model: "m"})
	if err != nil {
		t.Fatalf("Resume(%s): %v", id, err)
	}
	seeded := info.Updated

	fs := h.session(id)
	if err := h.sup.Send(ctx, id, "keep going"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := fs.waitStarted(t); got != "keep going" {
		t.Fatalf("dispatched text = %q, want %q", got, "keep going")
	}
	waitForStatus(t, h.sup, id, supervisor.StatusWorking)
	fs.finish(t, nil)
	waitForStatus(t, h.sup, id, supervisor.StatusNeedsInput)

	got := findInfo(roster(t, h.sup), id)
	if got == nil {
		t.Fatalf("session %s absent from the roster after a turn", id)
	}
	if got.Updated.Equal(seeded) {
		t.Errorf("Updated stayed at the seeded journal time %v after a real turn; a new turn must advance last-activity", seeded)
	}
}
