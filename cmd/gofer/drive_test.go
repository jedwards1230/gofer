package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// fakeDriver is a sessionDriver backed by a real event.Broker (so Events
// returns a genuine *event.Subscription) whose Prompt/Close outcomes the test
// controls. It lets us assert driveSession's error handling — in particular
// that a journal-write error surfaced by Close is not swallowed — without a
// live provider or an on-disk journal.
type fakeDriver struct {
	broker    *event.Broker
	promptErr error
	closeErr  error
}

func newFakeDriver() *fakeDriver { return &fakeDriver{broker: event.NewBroker()} }

func (f *fakeDriver) Events() *event.Subscription {
	return f.broker.Subscribe(event.FilterAll, 16)
}

func (f *fakeDriver) Prompt(_ context.Context, _ string) error {
	// Emit one settled turn (start → delta → finish) so the renderer has real
	// events to drain; the human renderer streams the delta text.
	f.broker.Publish(event.NewMessageStarted("fake", event.MessageText))
	f.broker.Publish(event.NewMessageDelta("fake", event.MessageText, "ok"))
	f.broker.Publish(event.NewMessageFinished("fake", event.MessageText, "ok"))
	f.broker.Publish(event.NewTurnFinished("fake", string(provider.StopEndTurn), provider.Usage{}))
	return f.promptErr
}

// Close closes the broker (ending driveSession's render loop) and returns the
// scripted error — modeling Runner.Close surfacing a journal-write failure the
// background consumer observed.
func (f *fakeDriver) Close() error {
	f.broker.Close()
	return f.closeErr
}

func (f *fakeDriver) ID() string { return "fake-id" }

// TestDriveSessionSurfacesCloseError is the regression test for the review
// finding: a journal-write error accumulated on the journaling goroutine and
// returned by Close must reach the caller (non-nil error → non-zero exit),
// never be silently dropped — otherwise a run reports success while the
// session did not fully persist, breaking the resumable-after-kill guarantee.
func TestDriveSessionSurfacesCloseError(t *testing.T) {
	writeErr := errors.New("session: append entry to journal: no space left on device")
	d := newFakeDriver()
	d.closeErr = writeErr

	var out, errBuf bytes.Buffer
	err := driveSession(context.Background(), d, "do a thing", false, &out, &errBuf)
	if err == nil {
		t.Fatal("driveSession returned nil, want the journal-write error surfaced from Close")
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("driveSession err = %v, want it to wrap %v", err, writeErr)
	}
}

// TestDriveSessionCleanRun confirms the happy path returns nil and renders the
// streamed turn, so the error path above is a real discriminator, not a
// constant failure.
func TestDriveSessionCleanRun(t *testing.T) {
	d := newFakeDriver()

	var out, errBuf bytes.Buffer
	if err := driveSession(context.Background(), d, "do a thing", false, &out, &errBuf); err != nil {
		t.Fatalf("driveSession returned %v, want nil on a clean run", err)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Errorf("rendered output = %q, want it to contain the streamed text %q", out.String(), "ok")
	}
}

// TestDriveSessionInterruptIsClean confirms a pure context cancellation is
// reported as an interrupt (nil error + a resume hint on stderr), while any
// non-cancel error joined alongside it is still surfaced — the exact
// distinction hasNonCancel draws.
func TestDriveSessionInterruptIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx.Err() == context.Canceled

	d := newFakeDriver()
	d.promptErr = context.Canceled

	var out, errBuf bytes.Buffer
	if err := driveSession(ctx, d, "do a thing", false, &out, &errBuf); err != nil {
		t.Fatalf("driveSession returned %v, want nil for a pure cancellation", err)
	}
	if !strings.Contains(errBuf.String(), "resume with `gofer resume fake-id`") {
		t.Errorf("stderr = %q, want the resume hint", errBuf.String())
	}

	// A journal-write error alongside the cancellation must NOT be masked.
	writeErr := errors.New("journal write failed")
	d2 := newFakeDriver()
	d2.promptErr = context.Canceled
	d2.closeErr = writeErr
	var out2, errBuf2 bytes.Buffer
	err := driveSession(ctx, d2, "do a thing", false, &out2, &errBuf2)
	if err == nil || !errors.Is(err, writeErr) {
		t.Fatalf("driveSession err = %v, want the journal-write error surfaced even on interrupt", err)
	}
}
