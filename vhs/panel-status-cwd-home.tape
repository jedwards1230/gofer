# gofer TUI — the /status command-panel tab's "Cwd: " row contracting $HOME
# to "~" (gofer#337), captured as a before/after PAIR. Companion to
# roster-cwd-home.tape: same technique, the OTHER render site.
#
# cannedCommandEnv's Cwd is the literal string "~/orchestration" — it never
# starts with a real absolute $HOME, so it can't exercise the contraction
# code. cwdHomeCommandEnv instead sets an absolute Cwd, and this tape runs
# the harness twice with a different HOME each time so HOME is the only
# variable between the two frames: before (HOME=/Users/nobody, no match,
# full absolute path) and after (HOME=/Users/justin, contracts to
# "~/orchestration").
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs
# a GIF plus two PNGs (before/after) under vhs/out/.

Output vhs/out/panel-status-cwd-home.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "HOME=/Users/nobody ./vhs/.bin/harness -scenario panel-status-cwd-home"
Enter
Show

Sleep 1s
Type "/status"
Enter
Sleep 1s
Screenshot vhs/out/panel-status-cwd-home-before.png
Sleep 100ms
Ctrl+C

Hide
Sleep 300ms
Type "HOME=/Users/justin ./vhs/.bin/harness -scenario panel-status-cwd-home"
Enter
Show

Sleep 1s
Type "/status"
Enter
Sleep 1s
Screenshot vhs/out/panel-status-cwd-home.png
Sleep 100ms
Ctrl+C
