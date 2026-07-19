# gofer TUI — issue #162: the roster header adopts a new default model in a
# LIVE process, with no restart.
#
# One continuous run of the real tui.App, driven by real keystrokes through
# stdin. It screenshots the header BEFORE the /model change and again AFTER,
# so the two PNGs are a before/after of the same process:
#
#   panel-model-daemon-refresh-before.png -> header reads claude-fable-5
#   panel-model-daemon-refresh-after.png  -> header reads claude-haiku-4-5
#                                            + a GREEN "daemon adopted it" note
#
# The scene is daemon-backed with an in-process stub gofer/hello probe
# (daemonRefreshCommandEnv), so it performs zero network IO — no daemon is
# dialed and no credential is read.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first).

Output vhs/out/panel-model-daemon-refresh.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-model-daemon-refresh"
Enter
Show

# BEFORE: the startup-cached header, showing the daemon's current default.
Sleep 1500ms
Screenshot vhs/out/panel-model-daemon-refresh-before.png

# Change the default: /model, then ↓↓ to highlight claude-haiku-4-5, Enter.
Type "/model"
Enter
Sleep 1s
Down
Down
Sleep 500ms
Enter

# AFTER: same process, no restart — the header must now read the new model.
Sleep 1500ms
Screenshot vhs/out/panel-model-daemon-refresh-after.png

Sleep 500ms
Ctrl+C
