# gofer TUI — the /status command-panel tab, attached to a session.
# Captures the Status tab reading real session identity (Session/Session ID/
# Model) once attached, plus both provider auth kinds (an Anthropic OAuth
# token and an OpenAI API key) in color — contrast vhs/panel.tape's
# no-session-attached frame.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the Status tab under vhs/out/.

Output vhs/out/status.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario status"
Enter
Show

Sleep 1s
Right
Sleep 800ms
Type "/status"
Enter
Sleep 1s
Screenshot vhs/out/status.png
Sleep 500ms
Ctrl+C
