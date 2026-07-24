// Package versionskew classifies how a client's build relates to the daemon it
// connected to. It is the single comparison the two client↔daemon skew
// surfaces share: the stderr warning on the run/resume path
// (cmd/gofer.warnVersionSkew) and the roster banner on the TUI path
// (internal/tui.Overview). Keeping the decision in one leaf package means the
// two surfaces can never drift on which pairs count as skewed.
package versionskew

import "golang.org/x/mod/semver"

// Kind is how a daemon's build relates to the client's, reduced to the three
// cases the skew surfaces render differently.
type Kind int

const (
	// None is nothing a daemon restart fixes: the versions are equal, exactly
	// one side identified itself (unknown — never a false positive), or the
	// daemon is NEWER than the client (the client is the stale side, so telling
	// the user to restart the daemon would be the wrong fix).
	None Kind = iota
	// Older is the stale-daemon case: both versions are comparable semver and
	// the daemon's is strictly older than the client's — a restart picks up the
	// newer build.
	Older
	// Differs is a real but undirected skew: the versions differ but their order
	// can't be established because one or both are non-semver local builds
	// (e.g. "dev-<sha>"). The daemon is definitely a different build; whether it
	// is older is unknown.
	Differs
)

// Classify decides how the daemon's build relates to the client's. It never
// false-positives on an unknown: an empty version on either side, or two equal
// versions, is [None].
//
// When BOTH are valid semver it uses [semver.Compare] for an authoritative
// direction — release tags AND Go pseudo-versions (vX.Y.Z-0.<ts>-<sha>) are
// valid semver and order correctly — and only a strictly-older daemon is
// [Older]; a newer or equal-precedence daemon is [None]. Versions that differ
// but aren't both semver are [Differs].
func Classify(client, daemon string) Kind {
	if client == "" || daemon == "" || client == daemon {
		return None
	}
	if semver.IsValid(client) && semver.IsValid(daemon) {
		if semver.Compare(daemon, client) < 0 {
			return Older
		}
		// Daemon newer than, or equal precedence to, the client.
		return None
	}
	return Differs
}

// String renders a Kind for logs and test failure messages.
func (k Kind) String() string {
	switch k {
	case None:
		return "none"
	case Older:
		return "older"
	case Differs:
		return "differs"
	default:
		return "invalid"
	}
}
