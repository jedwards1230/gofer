# gofer TUI — auto-scroll while drag-selecting on the attach transcript
# (gofer#312): a click-drag held at the transcript's top edge scrolls
# a.scroll back through history and keeps extending the selection, instead of
# clamping at whatever was on screen when the drag started.
#
# Captured as a BEFORE/AFTER PAIR, same reasoning as transcript-select-all and
# roster-select-all: one frame of a highlighted screen shows only that
# something is inverted, not that a SCROLL happened. The pair is what makes
# "the drag actually moved the viewport, and the selection followed it" a
# reviewable claim rather than a description to take on faith — the BEFORE
# frame tails to "turn 39" with the identity header scrolled out of view (the
# exact shape the issue was reported against: "i can only select whats
# already on screen"), and the AFTER frame shows the header back on screen
# with early turns highlighted, while "turn 39" is nowhere in the frame at
# all — yet still part of the copied selection (see mouse_docscroll_test.go's
# TestDragAutoScrollExtendsSelectionAcrossBoundary for the same assertion
# made mechanically).
#
# THE CLICK/DRAG ITSELF IS NOT TYPED — it can't be. VHS's tape DSL has no
# mouse commands at all (Type and named key presses only; see `vhs new`'s own
# documentation block). So unlike every other tape here, this scene's harness
# code (transcript-scroll-select: scrollSelectApp/scrollSelectMouseScript,
# vhs/harness/main.go) drives the click + held-edge drag PROGRAMMATICALLY —
# Program.Send-ing a real tea.MouseClickMsg then sixty tea.MouseMotionMsg
# events at the content's absolute top row, on a fixed delay after process
# start — the same "inject state the tape can't type" technique
# autoCompactingApp already uses for its timer-fired compaction-start event.
# See scrollSelectMouseScript's doc for why clicking the row is enough to sit
# at the top edge from the very first motion event, needing no assumption
# about exactly which row the transcript happens to overflow to.
#
# TIMING: the harness's drag sequence starts scrollSelectDriverDelay (3s)
# after PROCESS START, not after this tape's own keystrokes — so the "before"
# screenshot below has to land before that, and the "after" screenshot has to
# wait past it (3s delay + 61 steps * 70ms ≈ 3s of drag ≈ 6.3s total). The
# Sleep durations here are sized with slack on both sides of that budget.
#
# 40 seeded turns (harness: scrollSelectHistory) is comfortably more than any
# reasonable terminal height needs to overflow — deliberately plain, legible
# content: the turn NUMBERS are what makes the before/after pair readable
# evidence that history actually scrolled back, not a color or layout change
# a viewer has to take on faith. Seeded through the broker's RETAINED backlog
# (vhsSupervisor.seed), so the transcript is already on screen the instant the
# tape attaches — no publish-vs-subscribe race to make the captured frame
# timing-dependent.
#
# Read-only: a drag-selection installs a client-side selectionState (plus
# a.scroll, also client-side) and never reaches the Supervisor, so this scene
# mutates nothing.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus the two PNGs under vhs/out/.

Output vhs/out/transcript-scroll-select.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario transcript-scroll-select"
Enter
Show

# Attach to sess-1 (the roster's first row) and let the retained backlog
# settle onto the transcript — tailed to "turn 39" with the identity header
# scrolled away, before the harness's own driver goroutine has fired.
Sleep 2s
Enter
Sleep 1500ms

Screenshot vhs/out/transcript-scroll-select-before.png

# Past scrollSelectDriverDelay (3s) plus the full 61-step drag (~3s more, at
# ~2.2s elapsed already) — comfortably clear of the driver's own finish.
Sleep 5s

Screenshot vhs/out/transcript-scroll-select.png
Sleep 200ms
Ctrl+C
