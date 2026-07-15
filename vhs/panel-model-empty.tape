# gofer TUI — the /model command-panel tab with zero authenticated providers.
# Captures the model picker's empty state (no credentialed models to choose
# from) plus its "/login" hint — contrast panel-model.tape's populated list.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the empty Model tab under vhs/out/.

Output vhs/out/panel-model-empty.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-model-empty"
Enter
Show

Sleep 1s
Type "/model"
Enter
Sleep 1s
Screenshot vhs/out/panel-model-empty.png
Sleep 500ms
Ctrl+C
