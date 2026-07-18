package main

import (
	"runtime/debug"
	"testing"
)

func buildInfo(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main:     debug.Module{Version: mainVersion},
		Settings: settings,
	}
}

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name           string
		ldflagsVersion string
		info           *debug.BuildInfo
		ok             bool
		want           string
	}{
		{
			name:           "ldflags set wins even with build info present",
			ldflagsVersion: "v1.2.3",
			info: buildInfo("v0.10.0",
				debug.BuildSetting{Key: "vcs.revision", Value: "abcdef0123456789"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "v1.2.3",
		},
		{
			name:           "real Main.Version used",
			ldflagsVersion: "dev",
			info:           buildInfo("v0.10.0"),
			ok:             true,
			want:           "v0.10.0",
		},
		{
			name:           "devel with clean revision -> dev-<sha>",
			ldflagsVersion: "dev",
			info: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.revision", Value: "abcdef0123456789abcdef"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "dev-abcdef012345",
		},
		{
			name:           "devel with dirty revision -> dev-<sha>-dirty",
			ldflagsVersion: "dev",
			info: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.revision", Value: "abcdef0123456789abcdef"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "dev-abcdef012345-dirty",
		},
		{
			name:           "short revision (<12 chars) used whole",
			ldflagsVersion: "dev",
			info: buildInfo("",
				debug.BuildSetting{Key: "vcs.revision", Value: "abc123"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "dev-abc123",
		},
		{
			name:           "short revision dirty",
			ldflagsVersion: "dev",
			info: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.revision", Value: "abc123"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "dev-abc123-dirty",
		},
		{
			name:           "exactly 12 chars used whole",
			ldflagsVersion: "dev",
			info: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.revision", Value: "0123456789ab"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "dev-0123456789ab",
		},
		{
			name:           "build info unavailable -> dev",
			ldflagsVersion: "dev",
			info:           nil,
			ok:             false,
			want:           "dev",
		},
		{
			name:           "no vcs.revision setting -> dev",
			ldflagsVersion: "dev",
			info:           buildInfo("(devel)"),
			ok:             true,
			want:           "dev",
		},
		{
			name:           "empty Main.Version with no vcs.revision -> dev",
			ldflagsVersion: "dev",
			info: buildInfo("",
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveVersion(tt.ldflagsVersion, tt.info, tt.ok)
			if got != tt.want {
				t.Errorf("resolveVersion(%q, info, %v) = %q, want %q",
					tt.ldflagsVersion, tt.ok, got, tt.want)
			}
		})
	}
}
