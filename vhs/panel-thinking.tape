# gofer TUI — the /thinking command-panel tab: the reasoning-effort adjuster
# (Runner.SetEffort) with its level list and the active level marked. Opened
# from the roster overview at gofer's own defaults — the same frame the
# app_panel_thinking.golden pins in plain Ascii, here in real color.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the Thinking tab under vhs/out/.

Output vhs/out/panel-thinking.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-thinking"
Enter
Show

Sleep 1s
Type "/thinking"
Enter
Sleep 1s
Screenshot vhs/out/panel-thinking.png
Sleep 500ms
Ctrl+C
