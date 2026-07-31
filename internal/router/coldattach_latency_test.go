package router

// Wall-clock regression test for issue #313: a cold attach — session/load on an
// OFFLINE session, which spawns a worker and rebuilds it from the journal — must
// complete in a small fraction of the settle bound, not burn it.
//
// WHY THIS IS A TEST AND NOT A BENCHMARK
//
// The repo's performance gate (scripts/bench.sh) measures allocs/op and B/op and
// deliberately never gates wall-clock. That gate is structurally blind to this
// entire bug class: #313 was two seconds spent WAITING ON A TIMER, which
// allocates nothing. No allocation benchmark could ever have caught it, and the
// composed attach benchmark added alongside this file cannot either — see its
// own comment. Time is the only axis that can see a timeout being burned, so
// this lives in the test suite where a wall-clock assertion is allowed.
//
// It is also why the pre-existing #137 coverage missed it: internal/daemon's
// settleload_test.go drives the real session/load handler but against a FAKE
// supervisor whose AwaitSettled is a stub, so it pins the ORDERING (the fold is
// read after the wait) while saying nothing about how long the real wait takes.
// This test uses the real router and a real spawned worker.
//
// THE ASSERTION'S SHAPE
//
// The failure this must catch is "burns the full bound" — a 100x signal, not a
// 10% one (measured: 2.02s before the fix, 21ms after). The threshold is
// therefore set well below the bound but with generous headroom over the real
// cost, so it cannot flake on a loaded CI runner while still failing loudly the
// moment the settle wait becomes unsatisfiable again.

import (
	"context"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// coldAttachBudget bounds a cold attach. It is half of
// [config.DefaultLoadSettleTimeout]: comfortably above the real cost (~21ms
// locally, ~100x margin) and comfortably below the bound, so burning the settle
// timeout fails this test no matter how slow the runner is.
const coldAttachBudget = config.DefaultLoadSettleTimeout / 2

// TestColdAttachDoesNotBurnSettleTimeout is issue #313's regression gate.
//
// Before the fix, session/load resumed the session and then waited for
// StatusNeedsInput — but a resumed-but-unprompted session derives StatusIdle, so
// the wait could never be satisfied and always ran to its 2s bound. This drives
// the same sequence handleSessionLoad drives (Resume → AwaitSettled → History)
// against a real offline session and asserts the whole thing fits in the budget.
//
// Mutation check: revert either half of the fix — settledStatus in methods.go,
// or settledInRoster in internal/supervisor — and this fails with an elapsed at
// or above the full DefaultLoadSettleTimeout.
func TestColdAttachDoesNotBurnSettleTimeout(t *testing.T) {
	sup := newResumeSupervisor(t)
	ctx := context.Background()
	dir := t.TempDir()

	id, _ := makeOfflineSession(t, sup, dir)
	if _, ok := sup.get(id); ok {
		t.Fatal("session still live after crash; cannot exercise the cold-attach path")
	}

	start := time.Now()

	info, err := sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: dir})
	if err != nil {
		t.Fatalf("cold resume: %v", err)
	}
	// Guard the premise: if a resumed session ever stops reporting Idle, this
	// test silently stops covering #313's path, so fail loudly instead.
	if info.Status != supervisor.StatusIdle {
		t.Fatalf("resumed session status = %v, want %v — this test no longer exercises the #313 path",
			info.Status, supervisor.StatusIdle)
	}

	// Wait for the handle's pushed roster cache to be SEEDED before timing the
	// settle wait. Without this the test is vacuous ~2 times in 3: an unseeded
	// cache makes awaitHandleSettled return via its `info == nil` degraded path,
	// which is fast whether or not the fix is present. Verified by mutation —
	// with the fix reverted, the unsynchronised version passed 2 of 3 runs.
	//
	// This does not weaken the assertion. It pins the test to the case that
	// actually hurt users (a seeded row reporting Idle) instead of letting a
	// scheduling race decide which path is measured.
	h, ok := sup.get(id)
	if !ok {
		t.Fatal("no handle registered after resume")
	}
	select {
	case <-h.seeded:
	case <-time.After(5 * time.Second):
		t.Fatal("worker roster cache was never seeded; cannot exercise the seeded settle path")
	}
	if cached := h.info.Load(); cached == nil || cached.Status != supervisor.StatusIdle {
		t.Fatalf("cached row = %+v, want a seeded row reporting %v — this test no longer exercises the #313 path",
			cached, supervisor.StatusIdle)
	}

	settleCtx, cancel := context.WithTimeout(ctx, config.DefaultLoadSettleTimeout)
	serr := sup.AwaitSettled(settleCtx, id)
	cancel()
	if serr != nil {
		t.Fatalf("AwaitSettled on a resumed session = %v, want nil: the wait is unsatisfiable again", serr)
	}

	if _, err := sup.History(ctx, id); err != nil {
		t.Fatalf("history: %v", err)
	}

	if elapsed := time.Since(start); elapsed > coldAttachBudget {
		t.Fatalf("cold attach took %s, want under %s (settle bound is %s) — the settle wait is burning its timeout again",
			elapsed, coldAttachBudget, config.DefaultLoadSettleTimeout)
	}
}

// TestWarmReattachDoesNotBurnSettleTimeout covers the case that is easy to get
// wrong, and that an early reading of #313 got wrong: re-attaching to an
// ALREADY-LIVE session was ALSO paying the full 2s.
//
// Resume itself is genuinely free on this path — it returns via s.get(id) in
// ~400ns without respawning anything. But handleSessionLoad does Resume THEN
// AwaitSettled, and a resumed session keeps deriving StatusIdle for its entire
// unprompted life. So every re-attach to a session that had been resumed and not
// yet prompted burned the timeout again, not just the first one. Measured on the
// in-process supervisor: composed warm attach 2.000760583s before the fix,
// 22.75µs after.
//
// Timing Resume alone would therefore be VACUOUS here — it was ~400ns both
// before and after the fix. The composed Resume+AwaitSettled sequence is the
// only shape that can see this, which is why this test measures both together.
func TestWarmReattachDoesNotBurnSettleTimeout(t *testing.T) {
	sup := newResumeSupervisor(t)
	ctx := context.Background()
	dir := t.TempDir()

	id, _ := makeOfflineSession(t, sup, dir)
	if _, err := sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: dir}); err != nil {
		t.Fatalf("cold resume: %v", err)
	}
	h, ok := sup.get(id)
	if !ok {
		t.Fatal("no handle registered after resume")
	}
	select {
	case <-h.seeded:
	case <-time.After(5 * time.Second):
		t.Fatal("worker roster cache was never seeded")
	}

	// The composed re-attach: Resume (live fast path) + the settle wait.
	start := time.Now()
	if _, err := sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: dir}); err != nil {
		t.Fatalf("warm resume: %v", err)
	}
	settleCtx, cancel := context.WithTimeout(ctx, config.DefaultLoadSettleTimeout)
	serr := sup.AwaitSettled(settleCtx, id)
	cancel()
	if serr != nil {
		t.Fatalf("AwaitSettled on a warm re-attach = %v, want nil", serr)
	}

	if elapsed := time.Since(start); elapsed > coldAttachBudget {
		t.Fatalf("warm re-attach took %s, want under %s — the settle wait is burning its timeout again",
			elapsed, coldAttachBudget)
	}
}
