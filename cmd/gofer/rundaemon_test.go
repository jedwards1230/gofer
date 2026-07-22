package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestRunRoutesThroughDaemon covers `gofer run`'s daemon path end to end: with
// a daemon reachable at --daemon, the prompt is driven through it (session/new
// + session/prompt) rather than the in-process path, and the streamed
// session/update text lands on stdout exactly as the in-process human
// renderer would show it.
func TestRunRoutesThroughDaemon(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)

	var out, errBuf bytes.Buffer
	got := run([]string{"run", "--daemon", addr, "hello there"}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run(run, daemon) = %d, want 0\nstdout: %s\nstderr: %s", got, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "Hello") || !strings.Contains(out.String(), "help you today") {
		t.Errorf("stdout = %q, want the faux provider's greeting rendered", out.String())
	}
	if !strings.Contains(errBuf.String(), "daemon session") {
		t.Errorf("stderr = %q, want a daemon-session announcement", errBuf.String())
	}
}

// TestRunDaemonModelFlagIgnoredWarning covers the documented -m deviation on
// the daemon path: passing -m alongside a reachable daemon still succeeds
// (the daemon's own default model wins), with a clear notice on stderr rather
// than a silently ignored flag.
func TestRunDaemonModelFlagIgnoredWarning(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)

	var out, errBuf bytes.Buffer
	got := run([]string{"run", "-m", "no-such-model", "--daemon", addr, "hi"}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run(run -m, daemon) = %d, want 0\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "-m is ignored") {
		t.Errorf("stderr = %q, want a notice that -m is ignored on the daemon path", errBuf.String())
	}
}

// TestRunFallsBackWithNoDaemon confirms `gofer run` still reaches the
// in-process path unchanged when --daemon names an unreachable address (the
// default in every other cmd/gofer test) — a bad model on the in-process path
// still fails the same way (exit 1) it always has.
func TestRunFallsBackWithNoDaemon(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("OPENAI_API_KEY", "")

	var out, errBuf bytes.Buffer
	got := run([]string{"run", "-m", "no-such-model", "--daemon", "127.0.0.1:1", "--root", root, "do a thing"},
		strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(run, no daemon) = %d, want 1 (in-process path's usual unknown-model failure)\nstderr: %s", got, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "-m is ignored") {
		t.Errorf("stderr = %q, want no daemon-path notice when no daemon is reachable", errBuf.String())
	}
}

// TestRunDaemonUnauthorizedIsHardError confirms a reachable-but-unauthorized
// daemon is a real error (exit 1, naming the auth problem), not a silent
// fallback to the in-process path — an operator misconfiguring --token should
// find out, not have their session silently rerouted.
func TestRunDaemonUnauthorizedIsHardError(t *testing.T) {
	addr := testDaemon(t, "the-real-token", fauxProvider)

	var out, errBuf bytes.Buffer
	got := run([]string{"run", "--daemon", addr, "--token", "wrong", "hi"}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(run, wrong token) = %d, want 1\nstdout: %s\nstderr: %s", got, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "unauthorized") {
		t.Errorf("stderr = %q, want it to name the auth problem", errBuf.String())
	}
}

// TestResumeRoutesThroughDaemon covers `gofer resume <id> <prompt>`'s daemon
// path: session/load reopens the session that was created directly through
// the daemon, then session/prompt continues it, rendering the same as run's
// daemon path.
func TestResumeRoutesThroughDaemon(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	sid := newLiveSession(t, context.Background(), addr, "")

	var out, errBuf bytes.Buffer
	// resume's flags must precede the positional id/prompt (flag.Parse stops
	// at the first non-flag token — the same convention run/resume's other
	// tests already follow for -m/--root).
	got := run([]string{"resume", "--daemon", addr, sid, "hi", "again"}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run(resume, daemon) = %d, want 0\nstdout: %s\nstderr: %s", got, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "Hello") {
		t.Errorf("stdout = %q, want the faux provider's greeting rendered", out.String())
	}
	if !strings.Contains(errBuf.String(), shortID(sid)) {
		t.Errorf("stderr = %q, want the resumed session's short id", errBuf.String())
	}
	// The daemon-session progress line must be prefixed "gofer resume:", not
	// the hardcoded "gofer run:" it used to print regardless of command.
	if !strings.Contains(errBuf.String(), "gofer resume: daemon session") {
		t.Errorf("stderr = %q, want the `gofer resume:` prefix on the daemon-session line", errBuf.String())
	}
}

// TestRunDaemonRootAndJSONNotices covers the --root and --json deviation
// notices on the daemon path: both flags are meaningless there (the daemon
// owns its own store, and --json changes the emitted JSON shape), so each must
// announce itself on stderr rather than be silently ignored.
func TestRunDaemonRootAndJSONNotices(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)

	var out, errBuf bytes.Buffer
	got := run([]string{"run", "--root", "/tmp/ignored", "--json", "--daemon", addr, "hi"},
		strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run(run --root --json, daemon) = %d, want 0\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "--root is ignored") {
		t.Errorf("stderr = %q, want a --root-ignored notice", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "ACP session/update JSON") {
		t.Errorf("stderr = %q, want a --json shape-change notice", errBuf.String())
	}
}

// TestRunLocalOptOutSkipsDaemon covers --local (and its --no-daemon alias):
// even with a daemon reachable at --daemon, --local forces the in-process
// path — no daemon-session line, no deviation notices, and -m/--root are
// honored (here that surfaces as the in-process unknown-model failure).
func TestRunLocalOptOutSkipsDaemon(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("OPENAI_API_KEY", "")

	for _, flagName := range []string{"--local", "--no-daemon"} {
		t.Run(flagName, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			got := run([]string{"run", flagName, "-m", "no-such-model", "--daemon", addr, "--root", root, "do a thing"},
				strings.NewReader(""), &out, &errBuf)
			// -m is honored in-process, so an unknown model fails fast (exit 1)
			// — proof the daemon at addr was never used despite being reachable.
			if got != 1 {
				t.Fatalf("run(run %s) = %d, want 1 (in-process unknown-model failure)\nstderr: %s", flagName, got, errBuf.String())
			}
			if strings.Contains(errBuf.String(), "daemon session") {
				t.Errorf("stderr = %q, want no daemon-session line under %s", errBuf.String(), flagName)
			}
			if strings.Contains(errBuf.String(), "is ignored") {
				t.Errorf("stderr = %q, want no deviation notices under %s", errBuf.String(), flagName)
			}
		})
	}
}

// TestResumeReadOnlyNeverTouchesDaemon confirms `gofer resume <id>` with no
// prompt (the read-only transcript view) never dials a daemon at all: an
// unreachable --daemon address must not affect it, since the transcript view
// reads the on-disk journal directly.
func TestResumeReadOnlyNeverTouchesDaemon(t *testing.T) {
	root := t.TempDir()

	var out, errBuf bytes.Buffer
	got := run([]string{"resume", "--daemon", "127.0.0.1:1", "--root", root, "no-such-session"},
		strings.NewReader(""), &out, &errBuf)
	// The transcript read fails (no such session on disk), but that failure
	// must come from the journal lookup, never a daemon dial attempt.
	if got != 1 {
		t.Fatalf("run(resume, read-only) = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "daemon") {
		t.Errorf("stderr = %q, want no daemon-related message from the read-only path", errBuf.String())
	}
}

// TestDriveDaemonSessionCancelSendsSessionCancel covers Ctrl-C during a
// daemon-driven turn end to end at the driveDaemonSession level (a real
// signal is out of reach from a test): cancelling ctx while a turn is
// genuinely in flight (blockingProvider) sends session/cancel and waits for
// the real, terminal PromptResponse (StopReasonCancelled) rather than
// abandoning the call locally — the "interrupted" hint on stderr proves the
// wait actually happened, not just that Call returned ctx.Err() early.
func TestDriveDaemonSessionCancelSendsSessionCancel(t *testing.T) {
	bp := newBlockingProvider()
	addr := testDaemon(t, "", func() provider.Provider { return bp })

	c, err := daemon.Dial(context.Background(), addr, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	driveDone := make(chan error, 1)
	var out, errBuf bytes.Buffer
	go func() {
		driveDone <- driveDaemonSession(ctx, c, "run", "", t.TempDir(), "hi", subagentLink{}, false, &out, &errBuf)
	}()

	<-bp.started // the turn's first model call is genuinely blocked in flight
	cancel()

	select {
	case err := <-driveDone:
		if err != nil {
			t.Fatalf("driveDaemonSession = %v, want nil (an interrupt is not a failure)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("driveDaemonSession did not return after cancellation — session/cancel not sent, or the daemon never answered")
	}

	if !strings.Contains(errBuf.String(), "interrupted") {
		t.Errorf("stderr = %q, want the interrupted/resume hint", errBuf.String())
	}
}
