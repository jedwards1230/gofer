# gofer overview/roster TUI — the roster screen with mixed session states.
# Captures the ● status markers in COLOR — yellow while working / awaiting
# input (incl. the ●2 pending count) vs green once finished — the state the
# marker redesign moved out of glyph shape and into color alone, so the Ascii
# golden tests can't see it.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the roster under vhs/out/.

Output vhs/out/roster-overview.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario roster-overview"
Enter
Show

Sleep 2s
Screenshot vhs/out/roster-overview.png
Sleep 500ms
Ctrl+C
