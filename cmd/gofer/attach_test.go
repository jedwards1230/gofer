package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestAttachRequiresDaemon asserts `gofer attach` fails clearly (never
// falling back to a local path) when no daemon is reachable — the dial
// happens before the interactive-terminal check (see runAttach's doc), so
// this is observable even from this non-TTY test harness.
func TestAttachRequiresDaemon(t *testing.T) {
	var out, errBuf bytes.Buffer
	got := run([]string{"attach", "--daemon", "127.0.0.1:1"}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(attach, no daemon) = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no gofer daemon running") {
		t.Errorf("stderr = %q, want a clear no-daemon message", errBuf.String())
	}
}

// TestAttachUnknownSessionRejected asserts an unresolvable <session>
// argument fails before ever reaching the interactive-terminal check —
// exercised via the exported dispatch, still with no real TTY needed since
// the failure happens on the resolveSessionID call.
func TestAttachUnknownSessionRejected(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)

	var out, errBuf bytes.Buffer
	got := run([]string{"attach", "--daemon", addr, "no-such-session"}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(attach, unknown session) = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no live session matches") {
		t.Errorf("stderr = %q, want a clear unresolved-session message", errBuf.String())
	}
}

// TestAttachNonInteractiveWithLiveSession drives runAttach all the way
// through dialing the daemon and resolving a real live session id, stopping
// only at the interactive-terminal requirement (this test harness's stdout
// is a *bytes.Buffer, never a TTY) — covering everything up to but not
// including tea.NewProgram.Run(), per docs/TESTING.md's no-PTY rule.
func TestAttachNonInteractiveWithLiveSession(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	sid := newLiveSession(t, context.Background(), addr, "")

	var out, errBuf bytes.Buffer
	got := run([]string{"attach", "--daemon", addr, sid}, strings.NewReader(""), &out, &errBuf)
	if got != 2 {
		t.Fatalf("run(attach, live session, non-tty) = %d, want 2 (usage: needs a real terminal)\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "interactive terminal") {
		t.Errorf("stderr = %q, want the interactive-terminal requirement", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), shortID(sid)) {
		t.Errorf("stderr = %q, want it to confirm the resolved session %s", errBuf.String(), shortID(sid))
	}
}

// TestAttachTooManyArgs asserts more than one positional argument is a
// usage error.
func TestAttachTooManyArgs(t *testing.T) {
	var out, errBuf bytes.Buffer
	got := run([]string{"attach", "one", "two"}, strings.NewReader(""), &out, &errBuf)
	if got != 2 {
		t.Fatalf("run(attach, two args) = %d, want 2\nstderr: %s", got, errBuf.String())
	}
}
