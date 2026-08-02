package tui

import "testing"

// TestContractHome tables the path-boundary behavior [contractHome] must get
// right: a contraction only fires when path lands ON home — equal to it, or
// home followed by a separator — never merely sharing it as a text prefix.
// Injecting home rather than reading the real $HOME keeps this deterministic
// on any machine and in CI.
func TestContractHome(t *testing.T) {
	const home = "/Users/justin"

	tests := []struct {
		name string
		path string
		home string
		want string
	}{
		{
			name: "exactly home",
			path: "/Users/justin",
			home: home,
			want: "~",
		},
		{
			name: "home with trailing separator",
			path: "/Users/justin/",
			home: home,
			want: "~/",
		},
		{
			name: "normal child",
			path: "/Users/justin/orchestration/repos/gofer",
			home: home,
			want: "~/orchestration/repos/gofer",
		},
		{
			name: "sibling sharing the prefix must NOT contract",
			path: "/Users/justinother/x",
			home: home,
			want: "/Users/justinother/x",
		},
		{
			name: "sibling with no further path segment must NOT contract",
			path: "/Users/justinother",
			home: home,
			want: "/Users/justinother",
		},
		{
			name: "path not under home at all",
			path: "/var/log/syslog",
			home: home,
			want: "/var/log/syslog",
		},
		{
			name: "empty path",
			path: "",
			home: home,
			want: "",
		},
		{
			name: "relative path",
			path: "orchestration/gofer",
			home: home,
			want: "orchestration/gofer",
		},
		{
			name: "home itself carries a trailing separator",
			path: "/Users/justin/orchestration",
			home: "/Users/justin/",
			want: "~/orchestration",
		},
		{
			name: "home is the filesystem root — trimming must not empty it",
			path: "/etc/hosts",
			home: "/",
			want: "~/etc/hosts",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := contractHome(tc.path, tc.home)
			if got != tc.want {
				t.Errorf("contractHome(%q, %q) = %q, want %q", tc.path, tc.home, got, tc.want)
			}
		})
	}
}

// TestDisplayHome checks displayHome's thin wrapper over contractHome: it
// resolves the real os.UserHomeDir(), so this only asserts the shape it
// can — a path that could not possibly be under whatever this machine's
// actual home is renders unchanged.
func TestDisplayHome(t *testing.T) {
	got := displayHome("/definitely/not/a/home/path/at/all")
	if got != "/definitely/not/a/home/path/at/all" {
		t.Errorf("displayHome unexpected contraction: got %q", got)
	}
	if got := displayHome(""); got != "" {
		t.Errorf("displayHome(\"\") = %q, want \"\"", got)
	}
}
