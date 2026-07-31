# gofer TUI — ctrl+a select-all on the ATTACH transcript: what "select the
# whole screen" covers on the screen that has an input box (issue #307).
#
# NOT a duplicate of roster-select-all, because the chrome under test is a
# strictly different set. App.transcriptRegion's own doc enumerates what the old
# clamp excluded, and the attach entry lists the input box AND ITS FRAMING RULES
# on top of the trailing status/usage footer — neither of which exists on the
# overview. So the roster pair demonstrates "identity header + dispatch bar now
# highlight" and demonstrates exactly nothing about the input box. If #307
# regressed on attach alone, every currently-tracked frame would still render
# correctly and no golden would move. That blind spot is this tape's whole
# justification.
#
# Captured as a BEFORE/AFTER PAIR, same reasoning as roster-select-all: the
# subject is a reverse-video colour change, and one frame of a highlighted
# screen shows only that something is inverted, not what changed.
#
# ctrl+a is pressed with an EMPTY input box, which is the gate: with text in it
# the same key stays the readline "move to line start" and nothing highlights
# (keyBinding.enabled — see docs/TUI.md). This tape therefore also documents the
# empty-box precondition by never typing anything first.
#
# Reuses the transcript-compacting scenario — it already seeds two complete
# turns with a tool call in each (harness: compactableHistory) through the
# broker's RETAINED backlog, so a real conversation with a populated usage
# footer is on screen the moment the tape attaches. Same reason
# roster-delete-confirm, roster-peek and roster-select-all all reuse
# panel-status-overview: a scenario is reused when only the driving keys differ.
# NOTE that scenario's compactHold is INERT here — this tape never dispatches
# /compact, so nothing ever blocks. Do not read it as load-bearing for this
# capture.
#
# Nothing in frame is time-varying: the compaction counter (the one element that
# would churn the tracked baseline, gofer#297) only exists while a /compact is
# in flight, and this tape never starts one. The seeded history carries fixed
# timestamps and a fixed usage total, so both PNGs are byte-stable across runs.
#
# Read-only: select-all installs a client-side selectionState and writes the
# clipboard. It never touches the Supervisor, so this scene mutates nothing.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus the two PNGs under vhs/out/.

Output vhs/out/transcript-select-all.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario transcript-compacting"
Enter
Show

# Attach to the selected session, then let the retained backlog settle onto the
# transcript before the first capture.
Sleep 2s
Enter
Sleep 2s

Screenshot vhs/out/transcript-select-all-before.png
Sleep 500ms
Ctrl+A
Sleep 1s
Screenshot vhs/out/transcript-select-all.png
Sleep 100ms
Ctrl+C
