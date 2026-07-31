# gofer TUI — what the roster looks like a QUARTER SECOND after the confirming
# ctrl+x. The companion to roster-delete-confirm.tape, which stops at the arm;
# this one presses again and photographs the result landing.
#
# Captured as a BEFORE/AFTER PAIR, and here the pair is the whole point. The
# subject is gofer#322: a roster-mutating op used to leave its stale row on
# screen until the next 1s poll happened to come round, so the mean wait between
# the key press and the row updating was ~500ms and the worst case ~1s. A
# destructive key that looks like it did nothing invites a second press. The op
# now refetches the roster the moment it lands.
#
# That is a TIMING change, which is exactly what a single frame cannot show and
# what the Ascii goldens are structurally blind to — they assert the text of a
# state, never how long it took to arrive. So the after-frame is taken at a
# FIXED 250ms: comfortably after the refetch, comfortably inside the 1s poll
# interval. Before the fix that frame still read "working"; it now reads the
# terminal state.
#
# The selected row (sess-1) is Working, so App.confirmDestroy dispatches KILL,
# not archive — the sibling op the issue asked to be fixed consistently. Kill
# keeps the journal (repo invariant #4), so the row stays on the roster as a
# terminal one rather than vanishing; vhsSupervisor.Kill models exactly that.
#
# Reuses the panel-status-overview scenario like roster-delete-confirm and
# roster-peek do — only the driving keys differ, so no new harness scenario is
# needed. It does need vhsSupervisor.Kill to actually mutate the canned roster:
# against a Supervisor whose roster never changes, both frames would be
# identical and the tape would be decoration.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus the two PNGs under vhs/out/.

Output vhs/out/roster-delete-lands.gif

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
Screenshot vhs/out/roster-delete-lands-before.png
Sleep 500ms
Ctrl+X
Sleep 500ms
Ctrl+X
Sleep 250ms
Screenshot vhs/out/roster-delete-lands.png
Sleep 100ms
Ctrl+C
