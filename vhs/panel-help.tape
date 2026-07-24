# gofer TUI — the /help command-panel tab: the keymap + slash-command
# reference rendered from the live command registry, opened from the roster
# overview. The same frame the app_panel_help.golden pins.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the Help tab under vhs/out/.

Output vhs/out/panel-help.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-help"
Enter
Show

Sleep 1s
Type "/help"
Enter
Sleep 1s
Screenshot vhs/out/panel-help.png
Sleep 500ms
Ctrl+C
