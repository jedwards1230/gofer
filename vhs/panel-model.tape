# gofer TUI — the /model command-panel tab with authenticated providers.
# Captures the model picker's populated list (models from the credentialed
# Anthropic OAuth + OpenAI API-key providers) and the ✓ active-model mark —
# contrast panel-model-empty.tape's zero-credential state.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the Model tab under vhs/out/.

Output vhs/out/panel-model.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-model"
Enter
Show

Sleep 1s
Type "/model"
Enter
Sleep 1s
Screenshot vhs/out/panel-model.png
Sleep 500ms
Ctrl+C
