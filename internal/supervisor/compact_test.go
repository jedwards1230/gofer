package supervisor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestCompact covers the explicit-command happy path: an idle session's
// Compact call reaches the SDK seam (the fake session's Compact) with the
// given instructions and publishes session.compacted to a subscriber.
func TestCompact(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	sub, err := h.sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := h.sup.Compact(ctx, entry.ID, "focus on the plan"); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := fs.compactCallCount(); got != 1 {
		t.Errorf("fake session's Compact call count = %d, want 1", got)
	}
	if got := fs.lastCompactInstructions(); got != "focus on the plan" {
		t.Errorf("fake session's last Compact instructions = %q, want %q", got, "focus on the plan")
	}
	assertEventKind(t, sub, event.KindSessionCompacted)
}

// TestCompactWhileRunning asserts Compact is refused with ErrRunning on a
// session with a turn in flight or queued work — unlike SetModel, it has the
// SAME idle-only restriction as Archive, matching runner.Runner.Compact's
// documented precondition (never call it while a Prompt is in flight).
func TestCompactWhileRunning(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if err := h.sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusWorking)

	err = h.sup.Compact(ctx, entry.ID, "")
	if !errors.Is(err, supervisor.ErrRunning) {
		t.Fatalf("Compact while running err = %v, want errors.Is ErrRunning", err)
	}
	if got := fs.compactCallCount(); got != 0 {
		t.Errorf("fake session's Compact call count = %d, want 0 (rejected before reaching the SDK)", got)
	}

	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
}

// TestCompactNotLive asserts an unknown session id surfaces ErrNotLive.
func TestCompactNotLive(t *testing.T) {
	h := newHarness(t)

	err := h.sup.Compact(context.Background(), "does-not-exist", "")
	if !errors.Is(err, supervisor.ErrNotLive) {
		t.Fatalf("Compact on unknown session err = %v, want errors.Is ErrNotLive", err)
	}
}

// TestCompactPropagatesSDKError asserts an SDK-side rejection (here,
// runner.ErrNothingToCompact, standing in for a session whose folded context
// is already empty) passes through errors.Is-able rather than being
// swallowed or replaced.
func TestCompactPropagatesSDKError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	fs.failCompact(runner.ErrNothingToCompact)

	err = h.sup.Compact(ctx, entry.ID, "")
	if !errors.Is(err, runner.ErrNothingToCompact) {
		t.Fatalf("Compact err = %v, want errors.Is runner.ErrNothingToCompact", err)
	}
}

// TestAutoCompactTriggersOverThreshold is the end-to-end trigger test: with
// a low threshold and the fake session scripted to report usage already over
// it, a settled turn (Send + finish) fires an automatic Compact — observable
// both as a reached SDK call (fs.compactCallCount) and as a session.compacted
// event reaching a live subscriber, satisfying the "must be visible in the
// transcript, never silent" constraint at the event-delivery layer this
// package owns (the TUI-side rendering is covered in internal/tui).
func TestAutoCompactTriggersOverThreshold(t *testing.T) {
	threshold := 0.5
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.Compaction = func() config.Compaction {
			return config.Compaction{ThresholdFraction: &threshold}
		}
	})
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	sub, err := h.sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// claude-haiku-4-5's registered ContextWindow is 200_000; 150_000 input
	// tokens is 75% — over the scripted 50% threshold.
	fs.setLastUsage("claude-haiku-4-5", provider.Usage{InputTokens: 150_000})

	if err := h.sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	fs.finish(t, nil)

	assertEventKind(t, sub, event.KindSessionCompacted)
	if got := fs.compactCallCount(); got != 1 {
		t.Errorf("fake session's Compact call count = %d, want 1 (auto-triggered)", got)
	}
}

// TestAutoCompactStaysUnderThreshold asserts a turn whose usage stays below
// the configured threshold never reaches Compact — the common case, so a
// normal session pays no summarization round trip it doesn't need.
func TestAutoCompactStaysUnderThreshold(t *testing.T) {
	threshold := 0.5
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.Compaction = func() config.Compaction {
			return config.Compaction{ThresholdFraction: &threshold}
		}
	})
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	// 50_000 / 200_000 = 25%, under the 50% threshold.
	fs.setLastUsage("claude-haiku-4-5", provider.Usage{InputTokens: 50_000})

	if err := h.sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	if got := fs.compactCallCount(); got != 0 {
		t.Errorf("fake session's Compact call count = %d, want 0 (under threshold)", got)
	}
}

// TestAutoCompactAtThresholdTriggers pins the boundary: usage exactly AT the
// threshold fires (>=, not >) — see [shouldAutoCompact]'s doc via
// config.Compaction.Threshold.
func TestAutoCompactAtThresholdTriggers(t *testing.T) {
	threshold := 0.5
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.Compaction = func() config.Compaction {
			return config.Compaction{ThresholdFraction: &threshold}
		}
	})
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	// Exactly 50% of claude-haiku-4-5's 200_000 window.
	fs.setLastUsage("claude-haiku-4-5", provider.Usage{InputTokens: 100_000})

	if err := h.sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	if got := fs.compactCallCount(); got != 1 {
		t.Errorf("fake session's Compact call count = %d, want 1 (usage at threshold triggers)", got)
	}
}

// TestAutoCompactDisabled asserts Compaction.Disabled turns the automatic
// trigger off entirely, even with usage far over the default threshold —
// the explicit /compact path (exercised by TestCompact) stays available
// either way, since it does not consult this policy at all.
func TestAutoCompactDisabled(t *testing.T) {
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.Compaction = func() config.Compaction {
			return config.Compaction{Disabled: true}
		}
	})
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	// Deliberately at 100% of claude-haiku-4-5's 200_000 window — would
	// trigger under the default policy.
	fs.setLastUsage("claude-haiku-4-5", provider.Usage{InputTokens: 200_000})

	if err := h.sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	if got := fs.compactCallCount(); got != 0 {
		t.Errorf("fake session's Compact call count = %d, want 0 (auto-compaction disabled)", got)
	}
}

// TestAutoCompactUnknownContextWindowNeverTriggers asserts a model the SDK
// registry does not carry (ContextWindow unknown, 0) never auto-triggers, no
// matter how large InputTokens is — dividing by an unknown window would be a
// guess, not a measurement (see shouldAutoCompact's doc).
func TestAutoCompactUnknownContextWindowNeverTriggers(t *testing.T) {
	h := newHarness(t) // default policy: auto-compaction on, 85% threshold
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "totally-unregistered-model"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	fs.setLastUsage("totally-unregistered-model", provider.Usage{InputTokens: 1_000_000})

	if err := h.sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	if got := fs.compactCallCount(); got != 0 {
		t.Errorf("fake session's Compact call count = %d, want 0 (unknown context window)", got)
	}
}

// TestAutoCompactSkipsAfterCancelledTurn asserts a cancelled turn (Interrupt)
// never evaluates the trigger — a cancelled turn's usage may be partial or
// absent, so pump only checks after a CLEAN, non-cancelled settle.
func TestAutoCompactSkipsAfterCancelledTurn(t *testing.T) {
	threshold := 0.1 // deliberately tiny, so any real evaluation would fire
	h := newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.Compaction = func() config.Compaction {
			return config.Compaction{ThresholdFraction: &threshold}
		}
	})
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	fs.setLastUsage("claude-sonnet-5", provider.Usage{InputTokens: 100_000})

	if err := h.sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)

	if err := h.sup.Interrupt(ctx, entry.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	if got := fs.compactCallCount(); got != 0 {
		t.Errorf("fake session's Compact call count = %d, want 0 (cancelled turn never evaluated)", got)
	}
}

// overflowHarness is the shared setup for the failure-triggered compaction
// tests (gofer#279): one live session with a subscriber attached, prompted
// with "hello" and already blocked inside its first Prompt call. Every test
// below starts from exactly this state and differs only in what it releases
// that call with.
//
// The compaction policy is left at its default on purpose — the failure
// trigger does not consult it (see recoverFromContextOverflow's doc) — and
// LastUsage is never armed, so the THRESHOLD trigger cannot fire and any
// Compact these tests observe is unambiguously the failure trigger's.
func overflowHarness(t *testing.T) (*supervisor.Supervisor, string, *fakeSession, *event.Subscription) {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	sub, err := h.sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := h.sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := fs.waitStarted(t); got != "hello" {
		t.Fatalf("first dispatch text = %q, want %q", got, "hello")
	}
	return h.sup, entry.ID, fs, sub
}

// rosterUpdated returns id's current SessionInfo.Updated. The harness clock is
// a monotonic per-call counter, so successive reads are strictly ordered and a
// bump is observable without any wall-clock dependence.
func rosterUpdated(t *testing.T, sup *supervisor.Supervisor, id string) time.Time {
	t.Helper()
	roster, err := sup.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	for _, e := range roster {
		if e.ID == id {
			return e.Updated
		}
	}
	t.Fatalf("rosterUpdated: %s not in the roster", id)
	return time.Time{}
}

// awaitSessionErrorContaining drains sub until it observes a session.error
// whose message contains want, returning that message. It exists because the
// recovery path emits TWO session.errors in the failing case — its own notice
// and then whatever the retry ultimately returned — so [awaitSessionError],
// which returns the FIRST one it sees, cannot discriminate between them.
func awaitSessionErrorContaining(t *testing.T, sub *event.Subscription, want string) string {
	t.Helper()
	var seen []string
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed before a session.error containing %q; saw %q", want, seen)
			}
			if se, isErr := e.(event.SessionError); isErr {
				if strings.Contains(se.Err, want) {
					return se.Err
				}
				seen = append(seen, se.Err)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a session.error containing %q; saw %q", want, seen)
		}
	}
}

// TestContextOverflowCompactsAndRetries is the positive case: a turn the
// provider rejected with provider.ErrContextOverflow triggers a compaction
// and a re-dispatch of the SAME turn text, which then succeeds. The second
// waitStarted IS the retry — the fake's started channel receives once per
// dispatched turn — and the text it carries is what proves the retry re-issues
// the original prompt rather than an empty or rewritten one.
func TestContextOverflowCompactsAndRetries(t *testing.T) {
	sup, id, fs, _ := overflowHarness(t)

	// Captured while the ORIGINAL turn is still in flight, so the comparison
	// below spans exactly the recovery.
	beforeRetry := rosterUpdated(t, sup, id)

	fs.finish(t, provider.ErrContextOverflow)

	if got := fs.waitStarted(t); got != "hello" {
		t.Errorf("retry dispatch text = %q, want %q (the same turn, re-issued)", got, "hello")
	}
	// The retry is a fresh dispatch and must bump Updated like any other,
	// otherwise a slow recovery freezes the roster row's age at the original
	// dispatch and the session reads as stalled while it is working. Safe to
	// sample here: the bump is sequenced before the Prompt call waitStarted
	// observed.
	if afterRetry := rosterUpdated(t, sup, id); !afterRetry.After(beforeRetry) {
		t.Errorf("roster Updated across the recovery = %v, want it advanced past %v (the retry is a dispatch)",
			afterRetry, beforeRetry)
	}
	fs.finish(t, nil)
	waitForStatus(t, sup, id, supervisor.StatusNeedsInput)

	if got := fs.compactCallCount(); got != 1 {
		t.Errorf("Compact call count = %d, want 1 (one failure-triggered compaction)", got)
	}
	if got := fs.callCount(); got != 2 {
		t.Errorf("Prompt call count = %d, want 2 (original + one retry)", got)
	}
	if err := sup.LastError(id); err != nil {
		t.Errorf("LastError after a recovered turn = %v, want nil", err)
	}
}

// TestContextOverflowRetriesExactlyOnce is the bound, and the test that
// matters most here: a retry that ALSO overflows must not compact again or
// dispatch a third turn — compact-and-retry against a prompt that still does
// not fit is an infinite loop that burns tokens and reads as a hang.
//
// The bound is asserted by waiting for the session to settle back to
// needs-input, which only happens once the pump has fully unwound the turn.
// A regression that loops never reaches that state (the third Prompt would
// block on the fake's advance channel with nobody sending), so waitForStatus's
// own deadline turns the hang into a failure rather than a hung test — and the
// call counts below then pin the exact number of dispatches, which a bare
// timeout could not.
func TestContextOverflowRetriesExactlyOnce(t *testing.T) {
	sup, id, fs, sub := overflowHarness(t)

	fs.finish(t, provider.ErrContextOverflow)
	fs.waitStarted(t)
	fs.finish(t, provider.ErrContextOverflow) // the retry overflows too

	waitForStatus(t, sup, id, supervisor.StatusNeedsInput)

	if got := fs.compactCallCount(); got != 1 {
		t.Errorf("Compact call count = %d, want 1 (the retry must not compact again)", got)
	}
	if got := fs.callCount(); got != 2 {
		t.Errorf("Prompt call count = %d, want 2 (original + exactly one retry, no third)", got)
	}
	// The second overflow surfaces as an ordinary turn failure rather than
	// being swallowed by the recovery that provoked it.
	awaitSessionErrorContaining(t, sub, provider.ErrContextOverflow.Error())
	if err := sup.LastError(id); !errors.Is(err, provider.ErrContextOverflow) {
		t.Errorf("LastError = %v, want errors.Is provider.ErrContextOverflow", err)
	}
}

// TestContextOverflowEmitsVisibleNotice pins the transcript sequence: the
// notice lands BEFORE the session.compacted block, so a user reads "your
// context overflowed, I compacted and retried" rather than watching the
// session silently skip a beat — the triggering rejection produced no output
// of its own, so without this emit nothing at all marks it.
//
// It asserts the notice's CONTENT, not merely that some session.error
// appeared: an emit that only echoed the provider's rejection text would
// satisfy a kind-only assertion while telling the user nothing about the
// recovery.
func TestContextOverflowEmitsVisibleNotice(t *testing.T) {
	sup, id, fs, sub := overflowHarness(t)

	fs.finish(t, provider.ErrContextOverflow)
	fs.waitStarted(t)
	fs.finish(t, nil)
	waitForStatus(t, sup, id, supervisor.StatusNeedsInput)

	var kinds []string
	var notices []string
	deadline := time.After(2 * time.Second)
drain:
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed before session.compacted; kinds = %q", kinds)
			}
			kinds = append(kinds, e.Kind())
			if se, isErr := e.(event.SessionError); isErr {
				notices = append(notices, se.Err)
			}
			if e.Kind() == event.KindSessionCompacted {
				break drain
			}
		case <-deadline:
			t.Fatalf("timed out before session.compacted; kinds = %q", kinds)
		}
	}

	if len(kinds) < 2 || kinds[len(kinds)-2] != event.KindSessionError {
		t.Fatalf("event kinds = %q, want a %s immediately before the %s block",
			kinds, event.KindSessionError, event.KindSessionCompacted)
	}
	if len(notices) != 1 {
		t.Fatalf("session.error messages before compaction = %q, want exactly the recovery notice", notices)
	}
	notice := notices[0]
	for _, want := range []string{"context window", "compact", "retry"} {
		if !strings.Contains(notice, want) {
			t.Errorf("recovery notice = %q, want it to mention %q (what happened AND what is being done)", notice, want)
		}
	}
	if notice == provider.ErrContextOverflow.Error() {
		t.Errorf("recovery notice = %q, want it distinguishable from a bare re-emit of the provider rejection", notice)
	}
}

// TestNonOverflowPromptErrorNeverCompacts is the negative that pairs with
// TestContextOverflowCompactsAndRetries: an ordinary turn failure is emitted
// and left alone. Compaction is the remedy for one specific rejection, and
// firing it on every failure would summarize away a session's history because
// a tool timed out.
func TestNonOverflowPromptErrorNeverCompacts(t *testing.T) {
	sup, id, fs, sub := overflowHarness(t)

	fs.finish(t, errors.New("provider unreachable"))
	waitForStatus(t, sup, id, supervisor.StatusNeedsInput)

	if got := fs.compactCallCount(); got != 0 {
		t.Errorf("Compact call count = %d, want 0 (a non-overflow failure must not compact)", got)
	}
	if got := fs.callCount(); got != 1 {
		t.Errorf("Prompt call count = %d, want 1 (a non-overflow failure must not retry)", got)
	}
	awaitSessionErrorContaining(t, sub, "provider unreachable")
}

// TestContextOverflowNothingToCompactSurfacesOriginal covers the one
// compaction failure with its own answer: runner.ErrNothingToCompact means
// there is no history to shrink, so the overshoot is a single oversized
// payload in THIS turn and a retry would be rejected identically. No retry is
// dispatched, and the ORIGINAL rejection surfaces — "nothing to compact"
// describes the remedy that did not apply, not the user's actual problem.
//
// It also pins the SECOND notice. The first notice promises a compaction and a
// retry; on this branch neither happens, so without an explicit "nothing could
// be shrunk" line the transcript reads promise → raw rejection and leaves the
// user to guess whether the remedy silently failed. The three
// awaitSessionErrorContaining calls below assert ORDER as well as presence:
// the helper only ever drains forward, so a later call cannot match an earlier
// event.
func TestContextOverflowNothingToCompactSurfacesOriginal(t *testing.T) {
	sup, id, fs, sub := overflowHarness(t)
	fs.failCompact(runner.ErrNothingToCompact)

	fs.finish(t, provider.ErrContextOverflow)
	waitForStatus(t, sup, id, supervisor.StatusNeedsInput)

	if got := fs.callCount(); got != 1 {
		t.Errorf("Prompt call count = %d, want 1 (nothing to compact ⇒ no retry)", got)
	}
	if got := fs.compactCallCount(); got != 1 {
		t.Errorf("Compact call count = %d, want 1 (attempted once, refused by the SDK)", got)
	}

	awaitSessionErrorContaining(t, sub, "compacting the conversation and retrying")
	second := awaitSessionErrorContaining(t, sub, "nothing to compact")
	if !strings.Contains(second, "not retried") {
		t.Errorf("second notice = %q, want it to say the turn was not retried", second)
	}
	surfaced := awaitSessionErrorContaining(t, sub, provider.ErrContextOverflow.Error())
	if strings.Contains(surfaced, runner.ErrNothingToCompact.Error()) {
		t.Errorf("surfaced error = %q, want the original overflow alone, not the compaction refusal", surfaced)
	}
	if err := sup.LastError(id); !errors.Is(err, provider.ErrContextOverflow) {
		t.Errorf("LastError = %v, want errors.Is provider.ErrContextOverflow", err)
	}
	if err := sup.LastError(id); errors.Is(err, runner.ErrNothingToCompact) {
		t.Errorf("LastError = %v, want the compaction refusal NOT carried alongside the original", err)
	}
}

// TestContextOverflowCompactionFailureJoinsBothHalves covers the generic
// compaction-failure branch: the remedy could not run for a reason that is not
// "nothing to shrink". Both halves must reach the user — the turn did not fit,
// AND the compaction that would have fixed it failed — because either alone is
// misleading. The errors.Is assertion is the load-bearing one: classification
// has to survive errors.Join, which it does because stdlib errors.Is traverses
// every branch of a join.
func TestContextOverflowCompactionFailureJoinsBothHalves(t *testing.T) {
	sup, id, fs, sub := overflowHarness(t)
	fs.failCompact(errors.New("summarizer unreachable"))

	fs.finish(t, provider.ErrContextOverflow)
	waitForStatus(t, sup, id, supervisor.StatusNeedsInput)

	if got := fs.callCount(); got != 1 {
		t.Errorf("Prompt call count = %d, want 1 (a failed compaction must not retry)", got)
	}

	awaitSessionErrorContaining(t, sub, "compacting the conversation and retrying")
	surfaced := awaitSessionErrorContaining(t, sub, "summarizer unreachable")
	if !strings.Contains(surfaced, provider.ErrContextOverflow.Error()) {
		t.Errorf("surfaced error = %q, want it to carry the original overflow as well", surfaced)
	}
	err := sup.LastError(id)
	if !errors.Is(err, provider.ErrContextOverflow) {
		t.Errorf("LastError = %v, want errors.Is provider.ErrContextOverflow (classification survives the join)", err)
	}
}

// TestContextOverflowCancelledCompactionStaysSilent pins the sentence the join
// branch's comment makes: a compaction cancelled out from under the recovery
// contributes context.Canceled to the join, pump's existing emit filter sees it
// THROUGH the join, and the failure stays silent exactly as any other cancelled
// turn does. An Esc during a recovery should not leave an error in the
// transcript.
//
// fs.failCompact(context.Canceled) is a stand-in: the fake's Compact never
// blocks, so there is no window to cancel it in (see fakeSession.Compact).
//
// The silence half is paired with a positive so it cannot pass vacuously: a
// SECOND turn is driven to a known error, and that error is the sentinel the
// drain runs to. Everything the recovery emitted has necessarily been delivered
// before it, so "only the notice, then the sentinel" is a real observation
// rather than a quiet timeout.
func TestContextOverflowCancelledCompactionStaysSilent(t *testing.T) {
	sup, id, fs, sub := overflowHarness(t)
	fs.failCompact(context.Canceled)

	fs.finish(t, provider.ErrContextOverflow)
	waitForStatus(t, sup, id, supervisor.StatusNeedsInput)

	if got := fs.callCount(); got != 1 {
		t.Errorf("Prompt call count = %d, want 1 (a cancelled compaction must not retry)", got)
	}
	err := sup.LastError(id)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("LastError = %v, want errors.Is context.Canceled", err)
	}
	if !errors.Is(err, provider.ErrContextOverflow) {
		t.Errorf("LastError = %v, want errors.Is provider.ErrContextOverflow too (both halves classify)", err)
	}

	fs.failCompact(nil)
	if err := sup.Send(context.Background(), id, "second"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	fs.finish(t, errors.New("sentinel-marker"))

	var got []string
	deadline := time.After(2 * time.Second)
drain:
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed before the sentinel; session.errors = %q", got)
			}
			if se, isErr := e.(event.SessionError); isErr {
				got = append(got, se.Err)
				if strings.Contains(se.Err, "sentinel-marker") {
					break drain
				}
			}
		case <-deadline:
			t.Fatalf("timed out before the sentinel; session.errors = %q", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("session.errors = %q, want exactly the recovery notice then the sentinel "+
			"(the cancelled compaction must emit nothing of its own)", got)
	}
	if !strings.Contains(got[0], "compacting the conversation and retrying") {
		t.Errorf("first session.error = %q, want the recovery notice", got[0])
	}
}

// TestInterruptDuringOverflowRetry proves the retry turn is interruptible.
// The original turnCtx is already cancelled by the time recovery starts, so
// the retry runs on a fresh context whose cancel func must be published as the
// session's live turnCancel — otherwise Interrupt would call the stale,
// already-cancelled one and an Esc during a recovery would be a silent no-op,
// leaving the user with no way to stop the turn.
//
// waitForStatus is what proves the cancellation actually landed: the fake's
// Prompt only returns on ctx.Done or an explicit finish, and this test never
// finishes the retry, so the session can only reach needs-input by being
// cancelled.
func TestInterruptDuringOverflowRetry(t *testing.T) {
	sup, id, fs, _ := overflowHarness(t)
	ctx := context.Background()

	fs.finish(t, provider.ErrContextOverflow)
	fs.waitStarted(t) // the retry turn is now in flight

	if err := sup.Interrupt(ctx, id); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitForStatus(t, sup, id, supervisor.StatusNeedsInput)

	if got := fs.callCount(); got != 2 {
		t.Errorf("Prompt call count = %d, want 2 (original + the interrupted retry)", got)
	}
	if err := sup.LastError(id); !errors.Is(err, context.Canceled) {
		t.Errorf("LastError = %v, want errors.Is context.Canceled", err)
	}
}
