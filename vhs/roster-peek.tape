# gofer TUI — the peek card: a roster-only session summary, opened with Space
# on an empty dispatch bar. Peek does NOT subscribe to the session's event
# stream (the subscription is established only on attach), so this frame is
# the card rendered purely from the roster snapshot.
#
# Space, not Enter: on the overview, enter/→ OPEN the session (attach) and
# space peeks it — see screenKeymap() in internal/tui/keymap.go. Driving this
# tape with Enter captures an empty attached transcript, not the peek card.
#
# Reuses the panel-status-overview scenario — it already builds the real
# tui.App over the canned two-session roster, and this tape just drives it to
# a different screen, so no new harness scenario is needed.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG of the peek card under vhs/out/.

Output vhs/out/roster-peek.gif

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
Space
Sleep 1s
Screenshot vhs/out/roster-peek.png
Sleep 100ms
Ctrl+C
