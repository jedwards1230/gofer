package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestAuthCommandExitCodes locks the login/logout/auth exit-code contract
// (0 success incl. --help, 2 usage error) so a flag-parse failure or a help
// request never leaks the generic exit 1. Driven through run() so the whole
// dispatch + parse path is covered.
func TestAuthCommandExitCodes(t *testing.T) {
	root := t.TempDir()

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"login help long", []string{"login", "--help"}, 0},
		{"login help short", []string{"login", "-h"}, 0},
		{"login help after positional", []string{"login", "anthropic", "--help"}, 0},
		{"logout help", []string{"logout", "--help"}, 0},
		{"auth help", []string{"auth", "--help"}, 0},
		{"login undefined flag", []string{"login", "--bogus", "--root", root}, 2},
		{"login flag missing value", []string{"login", "--root"}, 2},
		{"login unknown provider", []string{"login", "bogusprovider", "--root", root}, 2},
		{"login missing provider", []string{"login", "--root", root}, 2},
		{"logout undefined flag", []string{"logout", "--nope"}, 2},
		{"auth undefined flag", []string{"auth", "--nope"}, 2},
		{"auth extra subcommand", []string{"auth", "bogus", "--root", root}, 2},
		{"auth status ok", []string{"auth", "status", "--root", root}, 0},
		{"auth bare ok", []string{"auth", "--root", root}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			got := run(tc.args, strings.NewReader(""), &out, &errBuf)
			if got != tc.want {
				t.Errorf("run(%q) = %d, want %d\nstderr: %s", tc.args, got, tc.want, errBuf.String())
			}
		})
	}
}
