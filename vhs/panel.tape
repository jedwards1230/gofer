# gofer TUI — the command panel opened over the roster overview via /status.
# Captures the panel's general chrome (tab bar, rules, footer) composed over
# the roster it overlays, with no session attached — the Status tab's Session
# rows read "—" here, contrast vhs/status.tape's attached-session frame.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the opened panel under vhs/out/.

Output vhs/out/panel.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel"
Enter
Show

Sleep 1s
Type "/status"
Enter
Sleep 1s
Screenshot vhs/out/panel.png
Sleep 500ms
Ctrl+C
