# gofer TUI — the /config command-panel tab: the settings-registry search
# list (session.model, session.permission_mode, tui.roster_view, telemetry.*)
# at gofer's own defaults.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the Config tab under vhs/out/.

Output vhs/out/panel-config.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-config"
Enter
Show

Sleep 1s
Type "/config"
Enter
Sleep 1s
Screenshot vhs/out/panel-config.png
Sleep 500ms
Ctrl+C
