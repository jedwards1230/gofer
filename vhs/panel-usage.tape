# gofer TUI — the /usage command-panel tab opened from the roster overview
# (no session attached): the honest empty state, the same frame the
# app_panel_usage.golden pins. The attached-session token/cost mapping is
# asserted at the usageView level (usage_test.go), not shown here.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the Usage tab under vhs/out/.

Output vhs/out/panel-usage.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-usage"
Enter
Show

Sleep 1s
Type "/usage"
Enter
Sleep 1s
Screenshot vhs/out/panel-usage.png
Sleep 500ms
Ctrl+C
