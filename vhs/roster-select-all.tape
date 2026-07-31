# gofer TUI — ctrl+a select-all on the roster: what "select the whole screen"
# actually covers (issue #307).
#
# Captured as a BEFORE/AFTER PAIR on purpose. The thing under review is a
# COLOR CHANGE — reverse-video over the frame — and one frame of a highlighted
# screen cannot show what changed, only that something is inverted. The pair is
# what lets a reviewer check the specific claim this feature makes.
#
# What to look for in the AFTER frame, and why the Ascii goldens cannot check
# it: the reverse video must cover the rows OUTSIDE the roster body — the
# identity header (version, model · cwd, the awaiting/working/idle counts) at
# the top, and the dispatch bar plus its hint row at the bottom. Those are
# exactly the rows the previous transcript-region clamp left unpainted, and a
# golden test renders termenv.Ascii, so it cannot see a highlight at all.
#
# ctrl+a is pressed with an EMPTY dispatch bar, which is the gate: with text in
# the bar the same key stays the readline "move to line start" and nothing
# highlights (keyBinding.enabled — see docs/TUI.md). This tape therefore also
# documents the empty-bar precondition by never typing anything first.
#
# Read-only: select-all installs a client-side selectionState and writes the
# clipboard. It never touches the Supervisor, so this scene mutates nothing.
#
# Reuses the panel-status-overview scenario — it already builds the real
# tui.App over the canned two-session roster and this tape only drives it with
# a different key, so no new harness scenario is needed (same reason
# roster-delete-confirm and roster-peek reuse it).
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus the two PNGs under vhs/out/.

Output vhs/out/roster-select-all.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-status-overview"
Enter
Show

Sleep 2s
Screenshot vhs/out/roster-select-all-before.png
Sleep 500ms
Ctrl+A
Sleep 1s
Screenshot vhs/out/roster-select-all.png
Sleep 100ms
Ctrl+C
