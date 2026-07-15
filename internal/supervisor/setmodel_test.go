package supervisor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// infoFor returns id's roster row, failing the test if it is not live.
func infoFor(t *testing.T, sup *supervisor.Supervisor, id string) supervisor.SessionInfo {
	t.Helper()
	for _, e := range roster(t, sup) {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("session %s missing from roster", id)
	return supervisor.SessionInfo{}
}

// TestSetModel covers the happy path: a same-provider swap updates the
// roster's Model immediately and reaches the SDK setter (the fake session's
// SetModel) with the new model.
func TestSetModel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if err := h.sup.SetModel(ctx, entry.ID, "claude-opus-4-8"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	if got := infoFor(t, h.sup, entry.ID).Model; got != "claude-opus-4-8" {
		t.Errorf("roster Model = %q, want claude-opus-4-8", got)
	}
	if got := fs.lastSetModel(); got != "claude-opus-4-8" {
		t.Errorf("fake session's last SetModel arg = %q, want claude-opus-4-8", got)
	}
	if got := fs.setModelCallCount(); got != 1 {
		t.Errorf("fake session's SetModel call count = %d, want 1", got)
	}
}

// TestSetModelCrossProvider asserts a cross-provider target is rejected with
// [supervisor.ErrCrossProvider], WITHOUT reaching the SDK setter and without
// changing the roster's Model — the supervisor's own pre-check (via
// provider.Lookup) catches it before ever calling the fake session.
func TestSetModelCrossProvider(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	err = h.sup.SetModel(ctx, entry.ID, "gpt-5")
	if !errors.Is(err, supervisor.ErrCrossProvider) {
		t.Fatalf("SetModel cross-provider err = %v, want errors.Is ErrCrossProvider", err)
	}

	if got := infoFor(t, h.sup, entry.ID).Model; got != "claude-sonnet-5" {
		t.Errorf("roster Model after rejected SetModel = %q, want unchanged claude-sonnet-5", got)
	}
	if got := fs.setModelCallCount(); got != 0 {
		t.Errorf("fake session's SetModel call count = %d, want 0 (rejected before reaching the SDK)", got)
	}
}

// TestSetModelUnknownModel asserts an unregistered target model id is
// rejected with a clear error.
func TestSetModelUnknownModel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := h.sup.SetModel(ctx, entry.ID, "no-such-model"); err == nil {
		t.Fatal("SetModel with an unknown model: want an error, got none")
	}
}

// TestSetModelNotLive asserts an unknown session id surfaces
// [supervisor.ErrNotLive].
func TestSetModelNotLive(t *testing.T) {
	h := newHarness(t)

	err := h.sup.SetModel(context.Background(), "does-not-exist", "claude-opus-4-8")
	if !errors.Is(err, supervisor.ErrNotLive) {
		t.Fatalf("SetModel on unknown session err = %v, want errors.Is ErrNotLive", err)
	}
}

// TestSetModelEmptyModel asserts an empty model string is rejected with
// [supervisor.ErrEmptyModel] rather than forwarded to the SDK.
func TestSetModelEmptyModel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = h.sup.SetModel(ctx, entry.ID, "")
	if !errors.Is(err, supervisor.ErrEmptyModel) {
		t.Fatalf("SetModel with empty model err = %v, want errors.Is ErrEmptyModel", err)
	}
}

// TestSetModelWhileRunning asserts SetModel is valid on a session with a
// turn in flight — unlike Archive, it has no idle-only restriction, since
// the SDK setter is concurrency-safe and only the NEXT turn observes the
// change (see [supervisor.Supervisor.SetModel]'s doc).
func TestSetModelWhileRunning(t *testing.T) {
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

	if err := h.sup.SetModel(ctx, entry.ID, "claude-opus-4-8"); err != nil {
		t.Fatalf("SetModel while running: %v", err)
	}
	if got := infoFor(t, h.sup, entry.ID).Model; got != "claude-opus-4-8" {
		t.Errorf("roster Model while running = %q, want claude-opus-4-8", got)
	}

	// Let the in-flight turn settle cleanly.
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
}
