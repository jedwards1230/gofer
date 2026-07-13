package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDaemonRejectsNonLoopbackWithoutToken covers `gofer daemon`'s early
// daemon.ValidateListen call: a non-loopback --listen address with no token
// (flag or $GOFER_TOKEN) fails fast with a clean error, before any
// supervisor or model resolution work happens — this test sets no provider
// credentials at all, and would fail on model resolution instead if the
// validation didn't run first.
func TestDaemonRejectsNonLoopbackWithoutToken(t *testing.T) {
	t.Setenv("GOFER_TOKEN", "")

	var out, errBuf bytes.Buffer
	got := run([]string{"daemon", "--listen", "192.168.1.50:7333"}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(daemon, non-loopback, no token) = %d, want 1\nstdout: %s\nstderr: %s", got, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "refusing to bind") {
		t.Errorf("stderr = %q, want it to name the refusing-to-bind condition", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "--token") || !strings.Contains(errBuf.String(), "GOFER_TOKEN") {
		t.Errorf("stderr = %q, want it to name both remediations", errBuf.String())
	}
}

// TestDaemonAcceptsNonLoopbackWithGoferTokenEnv covers the $GOFER_TOKEN
// fallback feeding the same early validation: a non-loopback --listen with no
// --token flag but a non-empty $GOFER_TOKEN passes ValidateListen (it fails
// later, at model resolution, since this test sets no provider credentials —
// proof that it got PAST the listen check rather than stopping there).
func TestDaemonAcceptsNonLoopbackWithGoferTokenEnv(t *testing.T) {
	t.Setenv("GOFER_TOKEN", "the-token")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	var out, errBuf bytes.Buffer
	got := run([]string{"daemon", "--listen", "192.168.1.50:7333", "--root", t.TempDir()}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(daemon, non-loopback, GOFER_TOKEN set) = %d, want 1 (fails later, at model resolution)\nstdout: %s\nstderr: %s", got, out.String(), errBuf.String())
	}
	if strings.Contains(errBuf.String(), "refusing to bind") {
		t.Errorf("stderr = %q, want no refusing-to-bind error once GOFER_TOKEN is set", errBuf.String())
	}
}
