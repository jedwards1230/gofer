package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestWarnVersionSkew asserts the isolated warning helper names BOTH versions
// and writes to the writer it is given (never to os.Stderr directly), so the
// message is unit-testable and callers control the sink.
func TestWarnVersionSkew(t *testing.T) {
	var buf bytes.Buffer
	warnVersionSkew(&buf, "v1.2.3", "v1.0.0")

	out := buf.String()
	if !strings.Contains(out, "v1.2.3") {
		t.Errorf("warning missing the CLI version %q:\n%s", "v1.2.3", out)
	}
	if !strings.Contains(out, "v1.0.0") {
		t.Errorf("warning missing the daemon version %q:\n%s", "v1.0.0", out)
	}
	if !strings.Contains(out, "WARNING") {
		t.Errorf("warning is not loud (no WARNING marker):\n%s", out)
	}
	// It must name a concrete restart action, not a nonexistent subcommand.
	if !strings.Contains(out, "gofer daemon") {
		t.Errorf("warning omits the restart instruction:\n%s", out)
	}
}

// TestResolveCapturesEndpointVersion covers epVersion capture: resolve stashes
// the endpoint file's advertised version ONLY when addr actually settled on the
// file (the same trust guard as the file's token), and leaves it "" when a
// flag/env override picked a different address or no version was advertised.
func TestResolveCapturesEndpointVersion(t *testing.T) {
	type tc struct {
		name          string
		flagAddr      string
		envAddr       string
		fileAddr      string // "" means no endpoint file is written at all
		fileVersion   string
		wantEPVersion string
	}
	cases := []tc{
		{
			name:          "version captured when addr came from the file",
			fileAddr:      "10.0.0.3:9003",
			fileVersion:   "v0.9.0",
			wantEPVersion: "v0.9.0",
		},
		{
			name:          "no version captured when flag overrides the file's addr",
			flagAddr:      "10.0.0.1:9001",
			fileAddr:      "10.0.0.3:9003",
			fileVersion:   "v0.9.0",
			wantEPVersion: "",
		},
		{
			name:          "no version captured when env overrides the file's addr",
			envAddr:       "10.0.0.2:9002",
			fileAddr:      "10.0.0.3:9003",
			fileVersion:   "v0.9.0",
			wantEPVersion: "",
		},
		{
			name:          "empty version (old daemon) captured as empty",
			fileAddr:      "10.0.0.3:9003",
			fileVersion:   "",
			wantEPVersion: "",
		},
		{
			name:          "no file at all leaves version empty",
			wantEPVersion: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GOFER_DAEMON", c.envAddr)
			t.Setenv("GOFER_TOKEN", "")

			if c.fileAddr != "" {
				if err := daemon.WriteEndpoint("", daemon.Endpoint{
					Addr: c.fileAddr, PID: os.Getpid(), Version: c.fileVersion,
				}); err != nil {
					t.Fatalf("WriteEndpoint: %v", err)
				}
			}

			f := &daemonFlags{addr: c.flagAddr}
			f.resolve("")
			if f.epVersion != c.wantEPVersion {
				t.Errorf("epVersion = %q, want %q", f.epVersion, c.wantEPVersion)
			}
		})
	}
}

// TestDialDaemonWarnsOnSkew is the end-to-end check that the warning fires
// (once) on a real successful dial when the endpoint file's version differs
// from ours, stays silent when they match or the file advertised none, and —
// crucially — never breaks the command: the dial still returns a live client.
func TestDialDaemonWarnsOnSkew(t *testing.T) {
	cases := []struct {
		name        string
		fileVersion string
		wantWarn    bool
	}{
		{name: "mismatch warns", fileVersion: version + "-different", wantWarn: true},
		{name: "match is silent", fileVersion: version, wantWarn: false},
		{name: "unknown (old daemon) is silent", fileVersion: "", wantWarn: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr := testDaemon(t, "the-token", fauxProvider)
			hermeticDaemonEnv(t)
			if err := daemon.WriteEndpoint("", daemon.Endpoint{
				Addr: addr, Token: "the-token", PID: os.Getpid(), Version: c.fileVersion,
			}); err != nil {
				t.Fatalf("WriteEndpoint: %v", err)
			}

			var stderr bytes.Buffer
			f := &daemonFlags{}
			c2, err := dialDaemon(t.Context(), f, "", &stderr)
			if err != nil {
				t.Fatalf("dialDaemon: %v", err)
			}
			defer func() { _ = c2.Close() }()

			gotWarn := strings.Contains(stderr.String(), "WARNING")
			if gotWarn != c.wantWarn {
				t.Errorf("warning emitted = %v, want %v; stderr:\n%s", gotWarn, c.wantWarn, stderr.String())
			}
			if c.wantWarn {
				if !strings.Contains(stderr.String(), version) || !strings.Contains(stderr.String(), c.fileVersion) {
					t.Errorf("warning missing a version; stderr:\n%s", stderr.String())
				}
			}
		})
	}
}
