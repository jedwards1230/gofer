# gofer attach TUI — a clean turn with a bash tool call.
# Captures the real command in the tool header (from ToolCallFinished.Input),
# the block-to-block vertical rhythm, and a settled turn's status line.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF of the whole turn plus a PNG of the final frame under vhs/out/.

Output vhs/out/transcript-tool-call.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario transcript-tool-call"
Enter
Show

Sleep 2s
Screenshot vhs/out/transcript-tool-call-running.png
Sleep 4s
Screenshot vhs/out/transcript-tool-call.png
Sleep 500ms
Ctrl+C
