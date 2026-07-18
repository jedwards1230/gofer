package main

import (
	"fmt"
	"io"
	"runtime/debug"
	"sync"
)

// version is the gofer build version and the injection point for release
// builds, which override it via -ldflags "-X main.version=<v>" (see
// .github/workflows/release.yml, which stamps the exact tag). Local builds
// leave it at the "dev" sentinel; [effectiveVersion] then derives a more
// meaningful identifier from the build's embedded VCS metadata. Do NOT rename
// this var — the release workflow targets it by name, and it is the sole
// ldflags seam.
var version = "dev"

// effectiveVersionOnce memoizes [resolveVersion] so the potentially non-trivial
// [debug.ReadBuildInfo] call runs at most once per process, and every reporting
// site (CLI, TUI/daemon handshake headers) agrees on a single value.
var (
	effectiveVersionOnce sync.Once
	effectiveVersionVal  string
)

// effectiveVersion returns the build version to report everywhere: the CLI
// `gofer version` output and the Version field of every TUI/daemon handshake
// header. It resolves the raw ldflags [version] against the build's embedded
// info (see [resolveVersion]) and memoizes the result.
func effectiveVersion() string {
	effectiveVersionOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		effectiveVersionVal = resolveVersion(version, info, ok)
	})
	return effectiveVersionVal
}

// resolveVersion computes the reported build version from the ldflags-injected
// version and the build's embedded [debug.BuildInfo], in precedence order:
//
//  1. A release build stamps ldflagsVersion to the exact tag, so any value
//     other than the "dev" sentinel wins verbatim.
//  2. Otherwise consult BuildInfo. A real Main.Version — non-empty and not the
//     literal "(devel)" — is used as-is; this covers
//     `go install github.com/jedwards1230/gofer/cmd/gofer@v0.10.0`, which
//     stamps Main.Version to "v0.10.0".
//  3. Otherwise this is a local checkout (Main.Version is "(devel)" or empty),
//     so derive an identifier from the VCS build settings: "dev-<shortSHA>"
//     from vcs.revision (first 12 chars, or the whole revision if shorter),
//     with a "-dirty" suffix when vcs.modified is "true".
//
// When build info is unavailable (ok is false) or carries no vcs.revision
// setting (e.g. `go run` on a tree Go did not VCS-stamp), it falls back to the
// plain "dev" sentinel.
func resolveVersion(ldflagsVersion string, info *debug.BuildInfo, ok bool) string {
	if ldflagsVersion != "dev" {
		return ldflagsVersion
	}
	if !ok || info == nil {
		return "dev"
	}
	if mv := info.Main.Version; mv != "" && mv != "(devel)" {
		return mv
	}

	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return "dev"
	}

	short := revision
	if len(short) > 12 {
		short = short[:12]
	}
	out := "dev-" + short
	if modified == "true" {
		out += "-dirty"
	}
	return out
}

// runVersion prints the resolved build version.
func runVersion(w io.Writer) {
	_, _ = fmt.Fprintln(w, effectiveVersion())
}
