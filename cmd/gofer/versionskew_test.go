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
// message is unit-testable and callers control the sink. Both the "older" and
// "different build" phrasings must carry the versions, the loud marker, and a
// concrete restart action.
func TestWarnVersionSkew(t *testing.T) {
	for _, older := range []bool{true, false} {
		var buf bytes.Buffer
		warnVersionSkew(&buf, "v1.2.3", "v1.0.0", older)

		out := buf.String()
		if !strings.Contains(out, "v1.2.3") {
			t.Errorf("older=%v: warning missing the CLI version %q:\n%s", older, "v1.2.3", out)
		}
		if !strings.Contains(out, "v1.0.0") {
			t.Errorf("older=%v: warning missing the daemon version %q:\n%s", older, "v1.0.0", out)
		}
		if !strings.Contains(out, "WARNING") {
			t.Errorf("older=%v: warning is not loud (no WARNING marker):\n%s", older, out)
		}
		// It must name a concrete restart action, not a nonexistent subcommand.
		if !strings.Contains(out, "gofer daemon") {
			t.Errorf("older=%v: warning omits the restart instruction:\n%s", older, out)
		}
	}
	// The older phrasing must actually say the daemon is older, so the message
	// matches the classification and never tells the user the daemon is stale
	// when it is in fact newer.
	var older bytes.Buffer
	warnVersionSkew(&older, "v1.2.3", "v1.0.0", true)
	if !strings.Contains(older.String(), "older") {
		t.Errorf("older=true warning does not say the daemon is older:\n%s", older.String())
	}
}

// TestClassifyClientDaemonSkew is the pure core: which (cli, daemon) pairs warn,
// and how. It is the unit that must never false-positive on an unknown.
func TestClassifyClientDaemonSkew(t *testing.T) {
	cases := []struct {
		name        string
		cli, daemon string
		want        clientDaemonSkew
	}{
		{"equal releases are silent", "v0.3.1", "v0.3.1", skewNoWarn},
		{"equal pseudo-versions are silent", "v0.3.1-0.20260721163650-6661a1dcb818", "v0.3.1-0.20260721163650-6661a1dcb818", skewNoWarn},
		{"empty cli is silent (unknown)", "", "v0.3.1", skewNoWarn},
		{"empty daemon is silent (unknown)", "v0.3.1", "", skewNoWarn},
		{"both empty is silent", "", "", skewNoWarn},
		{"older release daemon warns as older", "v0.3.1", "v0.2.1", skewDaemonOlder},
		{"newer release daemon is silent (CLI is the stale side)", "v0.2.1", "v0.3.1", skewNoWarn},
		{
			// The exact scenario that triggered this feature: a client built from
			// a later commit than the long-running daemon, both Go pseudo-versions.
			"older pseudo-version daemon warns as older",
			"v0.3.1-0.20260721163650-6661a1dcb818",
			"v0.2.1-0.20260719230853-2aa711248af7",
			skewDaemonOlder,
		},
		{"newer pseudo-version daemon is silent", "v0.2.1-0.20260719230853-2aa711248af7", "v0.3.1-0.20260721163650-6661a1dcb818", skewNoWarn},
		{"differing dev builds warn as differs (order unknown)", "dev-6661a1dcb818", "dev-2aa711248af7", skewDaemonDiffers},
		{"release cli vs dev daemon warns as differs", "v0.3.1", "dev-2aa711248af7", skewDaemonDiffers},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyClientDaemonSkew(c.cli, c.daemon); got != c.want {
				t.Errorf("classifyClientDaemonSkew(%q, %q) = %d, want %d", c.cli, c.daemon, got, c.want)
			}
		})
	}
}

// TestDialDaemonWarnsFromHelloVersion covers the AUTHORITATIVE path: the daemon
// advertises its build via gofer/hello (no endpoint-file version at all), and
// dialDaemon warns based on that handshake — the leg that catches a stale daemon
// even when the client reached it via $GOFER_DAEMON or the loopback default
// rather than the endpoint file.
func TestDialDaemonWarnsFromHelloVersion(t *testing.T) {
	// A daemon that reports, via gofer/hello, a semver strictly older than this
	// CLI's — so the classifier resolves to skewDaemonOlder deterministically,
	// independent of the test binary's own effectiveVersion().
	cliVersion := effectiveVersion()
	addr := testDaemon(t, "", fauxProvider, "v0.0.1")
	hermeticDaemonEnv(t)
	t.Setenv("GOFER_DAEMON", addr)

	var stderr bytes.Buffer
	f := &daemonFlags{}
	c, err := dialDaemon(t.Context(), f, "", &stderr)
	if err != nil {
		t.Fatalf("dialDaemon: %v", err)
	}
	defer func() { _ = c.Close() }()

	out := stderr.String()
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("no skew warning for a hello-advertised older daemon; stderr:\n%s", out)
	}
	if !strings.Contains(out, "v0.0.1") || !strings.Contains(out, cliVersion) {
		t.Errorf("warning missing a version (cli %q, daemon v0.0.1); stderr:\n%s", cliVersion, out)
	}
	// f.epVersion is never set here (addr came from $GOFER_DAEMON, not the file),
	// proving the warning came from the gofer/hello handshake, not the file hint.
	if f.epVersion != "" {
		t.Errorf("epVersion = %q, want empty (addr came from env, not the endpoint file)", f.epVersion)
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
	// The fixtures MUST be built from effectiveVersion(), not the raw `version`
	// ldflags sentinel: dialDaemon compares against effectiveVersion(), which is
	// what a daemon stamps its endpoint file with. Building the "match is
	// silent" fixture from `version` would make that case — the exact regression
	// the effectiveVersion() plumbing exists to prevent — pass only by accident,
	// silently, on any build where the two happen to coincide (a test binary
	// today carries no vcs.* build settings, so they do).
	cliVersion := effectiveVersion()
	cases := []struct {
		name        string
		fileVersion string
		wantWarn    bool
	}{
		{name: "mismatch warns", fileVersion: cliVersion + "-different", wantWarn: true},
		{name: "match is silent", fileVersion: cliVersion, wantWarn: false},
		{name: "unknown (old daemon) is silent", fileVersion: "", wantWarn: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr := testDaemon(t, "the-token", fauxProvider)
			// hermeticDaemonHome, not hermeticDaemonEnv: this test's whole
			// subject is endpoint-FILE discovery, so it needs a fresh HOME to
			// write that file into but must leave $GOFER_DAEMON unset for
			// discovery to reach the file at all.
			hermeticDaemonHome(t)
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
				if !strings.Contains(stderr.String(), cliVersion) || !strings.Contains(stderr.String(), c.fileVersion) {
					t.Errorf("warning missing a version; stderr:\n%s", stderr.String())
				}
			}
		})
	}
}
