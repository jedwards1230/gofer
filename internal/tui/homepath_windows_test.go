//go:build windows

package tui

import "testing"

// TestContractHomeWindows exercises [contractHome] against native Windows
// paths (backslash-separated, drive-letter home) — CI here only runs
// ubuntu-latest, so [TestContractHome]'s POSIX examples never touch
// filepath.Separator's Windows branch; this file only builds/runs on an
// actual Windows GOOS, closing that gap for anyone building gofer there.
func TestContractHomeWindows(t *testing.T) {
	const home = `C:\Users\justin`

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "exactly home", path: `C:\Users\justin`, want: "~"},
		{name: "normal child", path: `C:\Users\justin\orchestration\repos\gofer`, want: `~\orchestration\repos\gofer`},
		{
			name: "sibling sharing the prefix must NOT contract",
			path: `C:\Users\justinother\x`,
			want: `C:\Users\justinother\x`,
		},
		{name: "path not under home at all", path: `C:\Windows\System32`, want: `C:\Windows\System32`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := contractHome(tc.path, home)
			if got != tc.want {
				t.Errorf("contractHome(%q, %q) = %q, want %q", tc.path, home, got, tc.want)
			}
		})
	}
}
