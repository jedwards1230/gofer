# gofer TUI — the armed delete confirm on the roster: what ctrl+x looks like
# between the first press and the second.
#
# Captured as a BEFORE/AFTER PAIR on purpose. The thing under review is a
# COLOR CHANGE — the focused row's highlight shifts from the neutral
# Highlight (#45475a) to the warm HighlightArmed (#5a4145) while a delete is
# armed — and a single frame of the armed state cannot show a change, only a
# color. The pair is what makes "just enough to signal a state change" a
# judgement a reviewer can actually make, the same reason
# panel-model-daemon-refresh screenshots its header twice.
#
# Both of the state's signals land in the same two frames:
#   - the focused row's full-width highlight (theme.RowHighlightArmedStyle)
#   - the dispatch-bar hint, which swaps the base
#     "enter open · space peek · tab toggle view · ctrl-x delete"
#     for "ctrl-x again to confirm deletion"
# The Ascii goldens pin the hint text; they cannot see the highlight color,
# which is exactly the gap this tape covers.
#
# ONE press only. The first ctrl+x is pure client state — it arms
# a.ctrlXArmed and does NOT touch the Supervisor (see App.confirmDestroy) —
# so this scene stays read-only and deletes nothing. A second press would act.
#
# Reuses the panel-status-overview scenario: it already builds the real
# tui.App over the canned two-session roster, and this tape just drives it
# with a different key, so no new harness scenario is needed (the same reason
# roster-peek reuses it).
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus the two PNGs under vhs/out/.

Output vhs/out/roster-delete-confirm.gif

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
Screenshot vhs/out/roster-delete-confirm-before.png
Sleep 500ms
Ctrl+X
Sleep 1s
Screenshot vhs/out/roster-delete-confirm.png
Sleep 100ms
Ctrl+C
