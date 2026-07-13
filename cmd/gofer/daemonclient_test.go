package main

import (
	"os"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestDaemonFlagsResolvePrecedence is the table test over
// [daemonFlags.resolve]'s documented precedence — flag > env
// ($GOFER_DAEMON/$GOFER_TOKEN) > the on-disk endpoint file > the loopback
// default — for both addr and token, including the rule that the file's
// token is used ONLY when its address is what resolve actually settled on
// for addr (no flag/env addr override). It exercises resolve("") — the
// default-root lookup every daemon-aware command with no --root of its own
// uses; [TestResolveHonorsRoot] covers the non-default-root case.
//
// Every case sets $HOME to a fresh t.TempDir() (isolating [daemon.ReadEndpoint]
// from a real ~/.gofer on the machine running the test) and explicitly sets
// both env vars (even to "") so nothing leaks in from the ambient test
// environment.
func TestDaemonFlagsResolvePrecedence(t *testing.T) {
	type tc struct {
		name                string
		flagAddr, flagToken string
		envAddr, envToken   string
		fileAddr, fileToken string // fileAddr == "" means no endpoint file is written at all
		wantAddr, wantToken string
	}

	cases := []tc{
		{
			name:      "flag wins over env, file, and default",
			flagAddr:  "10.0.0.1:9001",
			flagToken: "flag-token",
			envAddr:   "10.0.0.2:9002",
			envToken:  "env-token",
			fileAddr:  "10.0.0.3:9003",
			fileToken: "file-token",
			wantAddr:  "10.0.0.1:9001",
			wantToken: "flag-token",
		},
		{
			name:      "env wins over file and default when no flag",
			envAddr:   "10.0.0.2:9002",
			envToken:  "env-token",
			fileAddr:  "10.0.0.3:9003",
			fileToken: "file-token",
			wantAddr:  "10.0.0.2:9002",
			wantToken: "env-token",
		},
		{
			name:      "file wins over default when no flag or env",
			fileAddr:  "10.0.0.3:9003",
			fileToken: "file-token",
			wantAddr:  "10.0.0.3:9003",
			wantToken: "file-token",
		},
		{
			name:      "loopback default when nothing is set",
			wantAddr:  daemon.DefaultListenAddr,
			wantToken: "",
		},
		{
			name:      "env addr with no token anywhere never reads the file's token",
			envAddr:   "10.0.0.2:9002",
			fileAddr:  "10.0.0.3:9003",
			fileToken: "file-token",
			wantAddr:  "10.0.0.2:9002",
			wantToken: "",
		},
		{
			name:      "flag addr, even matching the file's, never reads the file's token",
			flagAddr:  "10.0.0.9:9009",
			fileAddr:  "10.0.0.9:9009",
			fileToken: "file-token",
			wantAddr:  "10.0.0.9:9009",
			wantToken: "",
		},
		{
			name:      "token flag overrides the file's token even when addr came from the file",
			flagToken: "flag-token",
			fileAddr:  "10.0.0.5:9005",
			fileToken: "file-token",
			wantAddr:  "10.0.0.5:9005",
			wantToken: "flag-token",
		},
		{
			name:      "token env overrides the file's token even when addr came from the file",
			envToken:  "env-token",
			fileAddr:  "10.0.0.6:9006",
			fileToken: "file-token",
			wantAddr:  "10.0.0.6:9006",
			wantToken: "env-token",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GOFER_DAEMON", c.envAddr)
			t.Setenv("GOFER_TOKEN", c.envToken)

			if c.fileAddr != "" {
				if err := daemon.WriteEndpoint("", daemon.Endpoint{
					Addr: c.fileAddr, Token: c.fileToken, PID: os.Getpid(),
				}); err != nil {
					t.Fatalf("WriteEndpoint: %v", err)
				}
			}

			f := &daemonFlags{addr: c.flagAddr, token: c.flagToken}
			gotAddr, gotToken := f.resolve("")
			if gotAddr != c.wantAddr {
				t.Errorf("resolve() addr = %q, want %q", gotAddr, c.wantAddr)
			}
			if gotToken != c.wantToken {
				t.Errorf("resolve() token = %q, want %q", gotToken, c.wantToken)
			}

			// resolve caches its result onto f, so a caller reading f.addr
			// after the call (e.g. for an error/notice message) sees the
			// resolved value.
			if f.addr != c.wantAddr {
				t.Errorf("f.addr after resolve() = %q, want %q (cached)", f.addr, c.wantAddr)
			}
		})
	}
}

// TestDialDaemonDiscoversEndpointFile is the integration-ish check that
// dialDaemon (not just resolve() in isolation) actually reaches a real
// daemon through the discovered endpoint file when no --daemon flag is
// given, and that an explicit flag still wins over a file naming a
// different (unreachable) address — proving the flag is genuinely
// consulted first, not merely preferred in the precedence doc.
func TestDialDaemonDiscoversEndpointFile(t *testing.T) {
	addr := testDaemon(t, "the-token", fauxProvider)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GOFER_DAEMON", "")
	t.Setenv("GOFER_TOKEN", "")

	if err := daemon.WriteEndpoint("", daemon.Endpoint{Addr: addr, Token: "the-token", PID: os.Getpid()}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	t.Run("no flag uses the discovered endpoint", func(t *testing.T) {
		f := &daemonFlags{}
		c, err := dialDaemon(t.Context(), f, "")
		if err != nil {
			t.Fatalf("dialDaemon: %v", err)
		}
		defer func() { _ = c.Close() }()
	})

	t.Run("explicit flag wins over a file naming a different, unreachable address", func(t *testing.T) {
		if err := daemon.WriteEndpoint("", daemon.Endpoint{Addr: "127.0.0.1:1", Token: "wrong"}); err != nil {
			t.Fatalf("WriteEndpoint: %v", err)
		}
		f := &daemonFlags{addr: addr, token: "the-token"}
		c, err := dialDaemon(t.Context(), f, "")
		if err != nil {
			t.Fatalf("dialDaemon with explicit flag: %v", err)
		}
		defer func() { _ = c.Close() }()
	})
}

// TestResolveHonorsRoot is the prod fix this change makes: a daemon and a
// client given the SAME non-default --root must discover each other, because
// resolve now reads the endpoint file from the caller's root rather than
// always the default ~/.gofer. It checks both directions — resolve(root)
// DOES see an endpoint file written under that root, and resolve("") does
// NOT see it (it looks at the default root instead, which — with $HOME
// pointed at an unrelated, empty tempdir — has no endpoint file at all).
func TestResolveHonorsRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // default root: empty, no endpoint file here
	t.Setenv("GOFER_DAEMON", "")
	t.Setenv("GOFER_TOKEN", "")

	root := t.TempDir()
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{
		Addr: "10.0.0.7:9007", Token: "root-token", PID: os.Getpid(),
	}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	t.Run("resolve(root) reads the endpoint file under root", func(t *testing.T) {
		f := &daemonFlags{}
		gotAddr, gotToken := f.resolve(root)
		if gotAddr != "10.0.0.7:9007" || gotToken != "root-token" {
			t.Errorf("resolve(%q) = (%q, %q), want (%q, %q)", root, gotAddr, gotToken, "10.0.0.7:9007", "root-token")
		}
	})

	t.Run(`resolve("") does not see a non-default root's endpoint file`, func(t *testing.T) {
		f := &daemonFlags{}
		gotAddr, gotToken := f.resolve("")
		if gotAddr != daemon.DefaultListenAddr || gotToken != "" {
			t.Errorf(`resolve("") = (%q, %q), want the loopback default (%q, "")`, gotAddr, gotToken, daemon.DefaultListenAddr)
		}
	})
}
