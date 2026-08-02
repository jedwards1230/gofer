package uicopy

import (
	"fmt"
	"time"
)

// Copy for the attach transcript's rendered blocks (internal/tui/model.go).
//
// Everything here is what the OPERATOR reads on screen. The near-duplicates in
// internal/tui/shell.go — `[exit %d]`, `[output truncated]`, the fold header —
// are the same words written into the MODEL's context and deliberately stay
// there; see the note above that group.

// TranscriptPermissionVerdict is an itemApprovalResolved block's line: the
// verdict word the request was answered with, prefixed with what was decided.
func TranscriptPermissionVerdict(verdict string) string { return "permission " + verdict }

// TranscriptWorking is the transient turn-in-flight indicator at the
// transcript tail. "working" rather than "thinking" because the turn may be
// running a tool, not only reasoning.
const TranscriptWorking = "working…"

// TranscriptCompacting is the transient compaction indicator at the transcript
// tail, counting whole elapsed seconds while a compaction runs. It shares
// [TranscriptWorking]'s muted grammar on purpose — both say "an operation you
// cannot see is running".
func TranscriptCompacting(elapsed time.Duration) string {
	return fmt.Sprintf("compacting context… (%s)", elapsed)
}

// TranscriptInterrupted marks a turn the operator deliberately stopped (Esc /
// session/cancel). It must keep reading as "you stopped this" rather than as a
// failure — the block beside it is styled as subdued chrome, not as an error.
const TranscriptInterrupted = "stopped"

// TranscriptToolAttribution is the origin clause a tool-call header wears when
// a subagent, not this session, made the call.
func TranscriptToolAttribution(agent string) string { return " · from the " + agent + " agent" }

// TranscriptMoreLines is the row a collapsed block body ends on, naming how
// many rows it hid rather than clipping them silently.
func TranscriptMoreLines(n int) string { return fmt.Sprintf("… +%d lines", n) }

// TranscriptBackgroundAgents is the background-agents block header. The
// caption points at the roster, which is where a child is stopped or drilled
// into.
func TranscriptBackgroundAgents(n int) string {
	if n == 1 {
		return fmt.Sprintf("%d background agent launched (↓ to manage)", n)
	}
	return fmt.Sprintf("%d background agents launched (↓ to manage)", n)
}

// TranscriptContextCompacted is the itemSessionCompacted block header. It
// names WHAT REPLACED WHAT rather than merely reporting that a swap happened.
func TranscriptContextCompacted(messages int) string {
	if messages == 1 {
		return fmt.Sprintf("Context compacted — %d message replaced with a summary", messages)
	}
	return fmt.Sprintf("Context compacted — %d messages replaced with a summary", messages)
}

// TranscriptCompactionUsage is the muted token/model line under a compaction
// header. The "~" and the "est." qualifier are load-bearing, not decoration:
// the "in" figure is the SUMMARIZER's own call, which measures the folded
// messages PLUS the compaction instructions, so it is context-plus-overhead
// rather than context alone.
func TranscriptCompactionUsage(inTokens, outTokens int, model string) string {
	return fmt.Sprintf("~%d in (est., incl. overhead) → %d out · %s", inTokens, outTokens, model)
}

// TranscriptShellRunAttribution is the origin clause a `!` / `!!` transcript
// block wears — the user-run counterpart of a tool call's
// [TranscriptToolAttribution]. It states, in words, that the command was run
// by YOU rather than leaving the reader to infer it from the sigil glyph.
const TranscriptShellRunAttribution = " · you ran this"

// TranscriptShellRunning is the only body row a `!` / `!!` block has while its
// command is still running, standing in for output that does not exist yet.
const TranscriptShellRunning = "running…"

// TranscriptShellExit is a shell run's non-zero exit row, and
// TranscriptShellTruncated its truncation row — both shown only when there is
// something abnormal to say.
//
// These are the OPERATOR's wording for what internal/tui/shell.go's
// contextBlock says to the model as `[exit %d]` / `[output truncated]`. Same
// words, two audiences, two homes: only this pair is copy.
func TranscriptShellExit(code int) string { return fmt.Sprintf("exit %d", code) }

// TranscriptShellTruncated marks output that hit the byte cap.
const TranscriptShellTruncated = "… output truncated"

// TranscriptAskUserQuestions summarizes a multi-question ask_user call, which
// has no single title to show in its place.
func TranscriptAskUserQuestions(n int) string {
	if n == 1 {
		return fmt.Sprintf("%d question", n)
	}
	return fmt.Sprintf("%d questions", n)
}
