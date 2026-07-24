package supervisor_test

// seteffort_test.go is setmodel_test.go's effort-axis twin: the same
// happy-path / rejection / not-live / while-running shape, plus the two places
// SetEffort deliberately DIFFERS from SetModel — an empty value is legal
// (it clears the level), and there is no cross-provider constraint at all.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSetEffort covers the happy path: a valid level updates the roster's
// Effort immediately and reaches the SDK setter (the fake session's SetEffort)
// with that level.
func TestSetEffort(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if got := infoFor(t, h.sup, entry.ID).Effort; got != "" {
		t.Fatalf("roster Effort at create = %q, want \"\" (no explicit level)", got)
	}

	if err := h.sup.SetEffort(ctx, entry.ID, provider.EffortHigh); err != nil {
		t.Fatalf("SetEffort: %v", err)
	}

	if got := infoFor(t, h.sup, entry.ID).Effort; got != provider.EffortHigh {
		t.Errorf("roster Effort = %q, want %q", got, provider.EffortHigh)
	}
	if got := fs.lastSetEffort(); got != provider.EffortHigh {
		t.Errorf("fake session's last SetEffort arg = %q, want %q", got, provider.EffortHigh)
	}
	if got := fs.setEffortCallCount(); got != 1 {
		t.Errorf("fake session's SetEffort call count = %d, want 1", got)
	}
}

// TestSetEffortEveryLevel walks the whole unified vocabulary, including the
// empty CLEAR value — the case with no SetModel analogue at all (an empty
// model is [supervisor.ErrEmptyModel]; an empty effort is a legitimate
// "unset the level and let the provider decide").
func TestSetEffortEveryLevel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	// The clear comes LAST so it has something to clear: setting high first and
	// then "" proves the empty value actually moves the roster back rather than
	// being a no-op that happens to match the create-time state.
	levels := []string{provider.EffortLow, provider.EffortMedium, provider.EffortHigh, ""}
	for i, level := range levels {
		if err := h.sup.SetEffort(ctx, entry.ID, level); err != nil {
			t.Fatalf("SetEffort(%q): %v", level, err)
		}
		if got := infoFor(t, h.sup, entry.ID).Effort; got != level {
			t.Errorf("roster Effort after SetEffort(%q) = %q", level, got)
		}
		if got := fs.lastSetEffort(); got != level {
			t.Errorf("fake session's last SetEffort arg after SetEffort(%q) = %q", level, got)
		}
		if got, want := fs.setEffortCallCount(), i+1; got != want {
			t.Errorf("fake session's SetEffort call count = %d, want %d", got, want)
		}
	}
}

// TestSetEffortInvalidLevel asserts a level outside the unified vocabulary is
// rejected with [supervisor.ErrInvalidEffort], WITHOUT reaching the SDK setter
// and without changing the roster — the supervisor's own ValidEffort pre-check
// catches it, so a caller can errors.Is against it rather than string-matching
// the SDK's plain error.
func TestSetEffortInvalidLevel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if err := h.sup.SetEffort(ctx, entry.ID, provider.EffortMedium); err != nil {
		t.Fatalf("SetEffort(medium): %v", err)
	}

	err = h.sup.SetEffort(ctx, entry.ID, "ultra")
	if !errors.Is(err, supervisor.ErrInvalidEffort) {
		t.Fatalf("SetEffort(\"ultra\") err = %v, want errors.Is ErrInvalidEffort", err)
	}
	if !strings.Contains(err.Error(), "ultra") {
		t.Errorf("SetEffort err = %q, want it to name the offending value", err.Error())
	}
	if got := infoFor(t, h.sup, entry.ID).Effort; got != provider.EffortMedium {
		t.Errorf("roster Effort after a rejected SetEffort = %q, want the unchanged %q", got, provider.EffortMedium)
	}
	if got := fs.setEffortCallCount(); got != 1 {
		t.Errorf("fake session's SetEffort call count = %d, want 1 (the rejected call must not reach the SDK)", got)
	}
}

// TestSetEffortCrossProviderIsFine is the deliberate NON-mirror of
// TestSetModelCrossProvider. A session's provider client is fixed at creation,
// which is what constrains a model swap — but effort is provider-agnostic
// vocabulary each backend projects onto its own wire format, so nothing about
// which provider a session runs on can make a level illegal. This test exists
// so a well-meaning "symmetry" refactor that copies SetModel's guard across
// fails immediately.
func TestSetEffortCrossProviderIsFine(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, model := range []string{"claude-sonnet-5", "gpt-5"} {
		t.Run(model, func(t *testing.T) {
			entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: model})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := h.sup.SetEffort(ctx, entry.ID, provider.EffortHigh); err != nil {
				t.Fatalf("SetEffort on a %s session: %v", model, err)
			}
			if got := infoFor(t, h.sup, entry.ID).Effort; got != provider.EffortHigh {
				t.Errorf("roster Effort = %q, want %q", got, provider.EffortHigh)
			}
		})
	}
}

// TestSetEffortSDKRejectionSurfaces asserts the supervisor propagates the SDK
// runner's own rejection (its non-reasoning-model check) rather than reporting
// success — and leaves the roster on the level the session actually has. The
// capability rule itself lives in the SDK by design (see
// [supervisor.Supervisor.SetEffort]), so this is the seam that proves gofer
// does not swallow its verdict.
func TestSetEffortSDKRejectionSurfaces(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	fs.failEffort(errors.New("runner: model does not support reasoning effort"))

	err = h.sup.SetEffort(ctx, entry.ID, provider.EffortHigh)
	if err == nil {
		t.Fatal("SetEffort with a failing SDK setter: want an error, got none")
	}
	if !strings.Contains(err.Error(), "does not support reasoning effort") {
		t.Errorf("SetEffort err = %q, want the SDK's own reason carried through", err.Error())
	}
	if got := infoFor(t, h.sup, entry.ID).Effort; got != "" {
		t.Errorf("roster Effort after a failed SetEffort = %q, want the unchanged \"\"", got)
	}
}

// TestSetEffortNotLive asserts an unknown session id surfaces
// [supervisor.ErrNotLive].
func TestSetEffortNotLive(t *testing.T) {
	h := newHarness(t)

	err := h.sup.SetEffort(context.Background(), "does-not-exist", provider.EffortLow)
	if !errors.Is(err, supervisor.ErrNotLive) {
		t.Fatalf("SetEffort on unknown session err = %v, want errors.Is ErrNotLive", err)
	}
}

// TestSetEffortWhileRunning asserts SetEffort is valid on a session with a turn
// in flight — like SetModel it has no idle-only restriction, since the SDK
// setter is concurrency-safe and only the NEXT turn observes the change.
func TestSetEffortWhileRunning(t *testing.T) {
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

	if err := h.sup.SetEffort(ctx, entry.ID, provider.EffortHigh); err != nil {
		t.Fatalf("SetEffort while running: %v", err)
	}
	if got := infoFor(t, h.sup, entry.ID).Effort; got != provider.EffortHigh {
		t.Errorf("roster Effort while running = %q, want %q", got, provider.EffortHigh)
	}

	// Let the in-flight turn settle cleanly.
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
}

// TestCreateSeedsEffortFromParams asserts the roster's Effort starts from the
// SAME value the runner seeds its own from (Options.Params.Thinking.Effort),
// not from a hardcoded "". Without this the roster would report "off" for a
// session that is in fact running at the level its creator asked for, and the
// picker's ✓ mark would point at the wrong row.
func TestCreateSeedsEffortFromParams(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{
		Cwd:    "/proj",
		Model:  "claude-sonnet-5",
		Params: provider.Params{Thinking: provider.Thinking{Enabled: true, Effort: provider.EffortMedium}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := entry.Effort; got != provider.EffortMedium {
		t.Errorf("created SessionInfo.Effort = %q, want the seeded %q", got, provider.EffortMedium)
	}
	if got := infoFor(t, h.sup, entry.ID).Effort; got != provider.EffortMedium {
		t.Errorf("roster Effort = %q, want the seeded %q", got, provider.EffortMedium)
	}
}
