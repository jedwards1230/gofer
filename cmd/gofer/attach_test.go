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

// TestAgentsIsAttachAlias covers `gofer agents` end to end: it dispatches
// through the exact same runAttach code path as `gofer attach` (main.go's
// combined "attach", "agents" case), so it must reproduce attach's behavior
// exactly — no-daemon hard error, resolved-session confirmation, and the
// too-many-args usage error — for the same inputs.
func TestAgentsIsAttachAlias(t *testing.T) {
	t.Run("no daemon reachable is a hard error, same as attach", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		got := run([]string{"agents", "--daemon", "127.0.0.1:1"}, strings.NewReader(""), &out, &errBuf)
		if got != 1 {
			t.Fatalf("run(agents, no daemon) = %d, want 1\nstderr: %s", got, errBuf.String())
		}
		if !strings.Contains(errBuf.String(), "no gofer daemon running") {
			t.Errorf("stderr = %q, want a clear no-daemon message", errBuf.String())
		}
		// The error-line prefix comes from reportCmdErr(cmd, ...), which must
		// use the invoked command name ("agents"), not a hardcoded "attach".
		if !strings.HasPrefix(errBuf.String(), "gofer agents:") {
			t.Errorf("stderr = %q, want it prefixed \"gofer agents:\"", errBuf.String())
		}
	})

	t.Run("no session argument opens the roster overview, requiring a real terminal", func(t *testing.T) {
		addr := testDaemon(t, "", fauxProvider)

		var out, errBuf bytes.Buffer
		got := run([]string{"agents", "--daemon", addr}, strings.NewReader(""), &out, &errBuf)
		if got != 2 {
			t.Fatalf("run(agents, no session, non-tty) = %d, want 2 (usage: needs a real terminal)\nstderr: %s", got, errBuf.String())
		}
		if !strings.Contains(errBuf.String(), "interactive terminal") {
			t.Errorf("stderr = %q, want the interactive-terminal requirement", errBuf.String())
		}
	})

	t.Run("a session argument resolves against the live roster, same as attach", func(t *testing.T) {
		addr := testDaemon(t, "", fauxProvider)
		sid := newLiveSession(t, context.Background(), addr, "")

		var out, errBuf bytes.Buffer
		got := run([]string{"agents", "--daemon", addr, sid}, strings.NewReader(""), &out, &errBuf)
		if got != 2 {
			t.Fatalf("run(agents, live session, non-tty) = %d, want 2\nstderr: %s", got, errBuf.String())
		}
		if !strings.Contains(errBuf.String(), shortID(sid)) {
			t.Errorf("stderr = %q, want it to confirm the resolved session %s", errBuf.String(), shortID(sid))
		}
	})

	t.Run("too many positional args is a usage error, same as attach", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		got := run([]string{"agents", "one", "two"}, strings.NewReader(""), &out, &errBuf)
		if got != 2 {
			t.Fatalf("run(agents, two args) = %d, want 2\nstderr: %s", got, errBuf.String())
		}
	})
}
