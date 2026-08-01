# gofer TUI — the ctrl+c double-tap quit confirm (gofer#314): what the roster
# looks like between the first press and the second.
#
# Captured as a BEFORE/AFTER PAIR on purpose, the same reason
# roster-delete-confirm.tape is — a single frame of the armed state cannot show
# a CHANGE. Unlike the ctrl+x confirm, this one's signal is TEXT, not just
# color: a.status switches from empty to "ctrl-c again to quit" (rendered in
# the theme's warn style), so the pair is what makes the change visible even
# though this text also happens to be readable in the Ascii goldens (this tape
# still earns its keep — see the file doc's "why capture this at all" note
# below).
#
# ONE press only. The first ctrl+c is pure client state — it arms
# a.quitArmed and does NOT reach tea.Quit (see App.confirmQuit) — so this
# scene never quits mid-recording. A second, confirming ctrl+c only follows
# the LAST screenshot, purely so the harness process exits promptly instead of
# idling out its 30s safety hold.
#
# Why a tape at all, if the Ascii golden can already see the text: the note is
# STYLED (theme.WarnStyle) and shares the render path with every other status
# note (setStatus/statusSeverity, issue #161) — a reviewer checking "is this
# the same warn-yellow every other caution note uses, not the danger-red an
# error would be" needs the real color, which the Ascii golden profile cannot
# show. The pair also documents, for anyone who has not read app.go, that the
# note appears and disappears around exactly one key rather than being some
# permanent chrome.
#
# Reuses the panel-status-overview scenario, same as roster-delete-confirm and
# roster-select-all: it already builds the real tui.App over the canned
# two-session roster, and this tape just drives it with a different key.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus the two PNGs under vhs/out/.

Output vhs/out/roster-quit-confirm.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-status-overview"
Enter
Show

Sleep 2s
Screenshot vhs/out/roster-quit-confirm-before.png
Sleep 500ms
Ctrl+C
Sleep 1s
Screenshot vhs/out/roster-quit-confirm.png
Sleep 100ms
Ctrl+C
