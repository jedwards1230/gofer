package supervisor_test

import (
	"context"
	"errors"
	"testing"

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
