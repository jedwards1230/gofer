package uicopy

// overview_render.go holds the roster overview's copy: the header counts, the
// stale-daemon banner, the empty/overflow states, the dispatch bar, and the
// long-form durations the peek card reads.
//
// The long-form duration units here are per-message plural functions, replacing
// internal/tui's shared plural(n, unit) helper — which appended an "s" to a noun
// its call sites passed in, encoding English's rule at every call site. Each
// message now owns its own plural form, which is what a move to CLDR plural
// categories needs (see the package doc).

import (
	"fmt"
	"strconv"
)

// OverviewAwaitingInput is the header count of sessions awaiting the user.
func OverviewAwaitingInput(n int) string { return fmt.Sprintf("%d awaiting input", n) }

// OverviewWorking is the header count of sessions with a turn in flight.
func OverviewWorking(n int) string { return fmt.Sprintf("%d working", n) }

// OverviewIdle is the header count of at-rest sessions, shown only when nonzero.
func OverviewIdle(n int) string { return fmt.Sprintf("%d idle", n) }

// OverviewCompleted is the header count of finished sessions.
func OverviewCompleted(n int) string { return fmt.Sprintf("%d completed", n) }

// OverviewDaemonStale is the header banner for a daemon older than this client.
func OverviewDaemonStale(daemon, client string) string {
	return fmt.Sprintf("⚠ daemon is stale (%s < %s) — press R to restart", daemon, client)
}

// OverviewDaemonDiffers is the header banner for a daemon on a different build.
func OverviewDaemonDiffers(daemon string) string {
	return fmt.Sprintf("⚠ daemon is a different build (%s) — press R to restart if it is stale", daemon)
}

// OverviewNoSessions is the roster's empty state.
const OverviewNoSessions = "No sessions yet — type below to start one."

// OverviewMoreBelow is the overflow note when rows fall off the bottom.
func OverviewMoreBelow(n int) string { return fmt.Sprintf("↓ %d more", n) }

// OverviewTokensUnit is the trailing unit of a tree row's token tally, joined
// to the compact count that precedes it ("214.7k tokens").
const OverviewTokensUnit = " tokens"

// OverviewDispatchPlaceholder is the empty dispatch bar's prompt.
const OverviewDispatchPlaceholder = "describe a task for a new session · ! for shell mode"

// OverviewConfirmDelete is the hint line while a ctrl-x delete awaits its confirm.
const OverviewConfirmDelete = "ctrl-x again to confirm deletion"

// OverviewHint is the dispatch bar's shortcut hint, shared by both rosters.
const OverviewHint = "enter open · space peek · tab toggle view · ctrl-x delete"

// OverviewHintStopAgents is the entry a tree roster appends to [OverviewHint].
const OverviewHintStopAgents = " · ctrl-t stop agents"

// OverviewHintShortcuts is the entry a flat roster appends to [OverviewHint].
const OverviewHintShortcuts = " · ? shortcuts"

// OverviewAgeNow is the compact age label for anything under a minute — the
// terse counterpart of [OverviewJustNow], sitting in a roster column beside
// "5m"/"3h"/"2d" rather than in a sentence.
const OverviewAgeNow = "now"

// OverviewJustNow is the long-form duration for anything under a minute.
const OverviewJustNow = "just now"

// Minutes renders a whole-minute duration in long form.
func Minutes(n int) string {
	if n == 1 {
		return strconv.Itoa(n) + " minute"
	}
	return strconv.Itoa(n) + " minutes"
}

// Hours renders a whole-hour duration in long form.
func Hours(n int) string {
	if n == 1 {
		return strconv.Itoa(n) + " hour"
	}
	return strconv.Itoa(n) + " hours"
}

// Days renders a whole-day duration in long form.
func Days(n int) string {
	if n == 1 {
		return strconv.Itoa(n) + " day"
	}
	return strconv.Itoa(n) + " days"
}
