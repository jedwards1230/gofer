# gofer TUI — the command panel opened over the roster overview via /status.
# Captures the panel's general chrome (tab bar, rules, footer) composed over
# the roster it overlays, with no session attached — the Status tab's Session
# rows read "—" here, contrast panel-status.tape's attached-session frame.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the opened panel under vhs/out/.

Output vhs/out/panel-status-overview.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-status-overview"
Enter
Show

Sleep 1s
Type "/status"
Enter
Sleep 1s
Screenshot vhs/out/panel-status-overview.png
Sleep 500ms
Ctrl+C
