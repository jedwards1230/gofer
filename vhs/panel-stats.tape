# gofer TUI — the /stats command-panel tab: the roster rollup (session-state
# counts across the canned two-session roster) opened from the overview, the
# same frame the app_panel_stats.golden pins.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the Stats tab under vhs/out/.

Output vhs/out/panel-stats.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-stats"
Enter
Show

Sleep 1s
Type "/stats"
Enter
Sleep 1s
Screenshot vhs/out/panel-stats.png
Sleep 100ms
Ctrl+C
