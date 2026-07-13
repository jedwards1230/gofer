package daemon_test

import (
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestValidateListen covers the loopback/non-loopback × token-empty/non-empty
// matrix: a token is required for every host that is not provably
// loopback-only, and never required for one that is — regardless of whether a
// token happens to be set anyway.
func TestValidateListen(t *testing.T) {
	cases := []struct {
		name       string
		addr       string
		isLoopback bool
	}{
		{"IPv4 loopback", "127.0.0.1:7333", true},
		{"IPv4 loopback other than .1", "127.5.5.5:7333", true},
		{"localhost hostname", "localhost:7333", true},
		{"IPv6 loopback", "[::1]:7333", true},
		{"IPv4 bind-all", "0.0.0.0:7333", false},
		{"IPv6 bind-all", "[::]:7333", false},
		{"empty host (bind-all shorthand)", ":7333", false},
		{"tailnet address", "100.64.1.5:7333", false},
		{"LAN address", "192.168.1.50:7333", false},
		{"unparseable host", "not-a-host:7333", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("empty token", func(t *testing.T) {
				err := daemon.ValidateListen(tc.addr, "")
				if tc.isLoopback {
					if err != nil {
						t.Errorf("ValidateListen(%q, \"\") = %v, want nil (loopback, no token required)", tc.addr, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("ValidateListen(%q, \"\") = nil, want an error (non-loopback requires a token)", tc.addr)
				}
				want := `refusing to bind non-loopback "` + tc.addr + `" without a bearer token; pass --token or set GOFER_TOKEN`
				if err.Error() != want {
					t.Errorf("ValidateListen(%q, \"\") error = %q, want %q", tc.addr, err.Error(), want)
				}
			})

			t.Run("non-empty token", func(t *testing.T) {
				if err := daemon.ValidateListen(tc.addr, "some-token"); err != nil {
					t.Errorf("ValidateListen(%q, \"some-token\") = %v, want nil", tc.addr, err)
				}
			})
		})
	}
}

// TestValidateListen_ErrorMessageContainsAddr guards the exact message shape
// callers (cmd/gofer's runDaemon) rely on for a clean CLI error, independent
// of the table above's own message check.
func TestValidateListen_ErrorMessageContainsAddr(t *testing.T) {
	err := daemon.ValidateListen("10.0.0.5:7333", "")
	if err == nil {
		t.Fatal("ValidateListen: want an error")
	}
	if !strings.Contains(err.Error(), `"10.0.0.5:7333"`) {
		t.Errorf("error = %q, want it to name the address", err.Error())
	}
	if !strings.Contains(err.Error(), "--token") || !strings.Contains(err.Error(), "GOFER_TOKEN") {
		t.Errorf("error = %q, want it to name both remediations", err.Error())
	}
}
