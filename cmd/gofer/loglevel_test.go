package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestParseLogLevel covers --log-level's string->slog.Level mapping: the four
// canonical names (case-insensitive, plus "warning" as an alias for "warn"),
// and a clean error for anything else.
func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"DEBUG", slog.LevelDebug, false},
		{"Info", slog.LevelInfo, false},
		{"trace", 0, true},
		{"", 0, true},
		{"verbose", 0, true},
	}
	for _, tc := range tests {
		got, err := parseLogLevel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseLogLevel(%q): got nil error, want one", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLogLevel(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestDaemonRejectsInvalidLogLevel covers runDaemon's --log-level validation:
// an unrecognized level fails fast (before any supervisor/model work) with a
// clean error naming the flag.
func TestDaemonRejectsInvalidLogLevel(t *testing.T) {
	t.Setenv("GOFER_LOG_LEVEL", "")

	var out, errBuf bytes.Buffer
	got := run([]string{"daemon", "--log-level", "verbose"}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(daemon, --log-level verbose) = %d, want 1\nstdout: %s\nstderr: %s", got, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "--log-level") {
		t.Errorf("stderr = %q, want it to name --log-level", errBuf.String())
	}
}

// TestDaemonAcceptsLogLevelEnvFallback covers the $GOFER_LOG_LEVEL fallback:
// no --log-level flag but a valid env value passes validation (the run fails
// later, at model resolution, since this test sets no provider credentials —
// proof it got PAST log-level parsing rather than stopping there).
func TestDaemonAcceptsLogLevelEnvFallback(t *testing.T) {
	t.Setenv("GOFER_TOKEN", "the-token")
	t.Setenv("GOFER_LOG_LEVEL", "debug")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	var out, errBuf bytes.Buffer
	got := run([]string{"daemon", "--listen", "192.168.1.50:7333", "--root", t.TempDir()}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(daemon, GOFER_LOG_LEVEL=debug) = %d, want 1 (fails later, at model resolution)\nstdout: %s\nstderr: %s", got, out.String(), errBuf.String())
	}
	if strings.Contains(errBuf.String(), "--log-level") {
		t.Errorf("stderr = %q, want no --log-level error once GOFER_LOG_LEVEL is a valid value", errBuf.String())
	}
}
