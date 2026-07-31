# gofer TUI — the /resume command-panel tab: the session picker with a fetched
# listing applied (an offline session no longer in the roster, plus a live
# one — see the harness ListSessions). The same frame the
# app_panel_resume.golden pins; opening it only lists, never resumes.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the Resume tab under vhs/out/.

Output vhs/out/panel-resume.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-resume"
Enter
Show

Sleep 1s
Type "/resume"
Enter
Sleep 1s
Screenshot vhs/out/panel-resume.png
Sleep 100ms
Ctrl+C
