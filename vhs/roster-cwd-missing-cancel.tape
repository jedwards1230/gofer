# gofer TUI — cancelling the missing-directory prompt: back to the overview,
# session untouched. The companion to roster-cwd-missing.tape, which stops at
# the prompt; this one answers it with Cancel and photographs the result.
#
# Captured as a BEFORE/AFTER PAIR, and here the pair IS the claim being made:
#
#   - before: the prompt open, its three choices offered.
#   - after:  Cancel ("2"), and the overview is back with sess-1 still on the
#     roster, still Working, still exactly as it was.
#
# "Nothing changed" is precisely what a single frame cannot demonstrate — an
# after-frame alone is just a roster. Against the before-frame it shows that
# the escape hatch really is inert: no row removed, no status changed, no
# error left on the footer. The Supervisor-side half of that claim (cancel
# records no op at all) is asserted by TestCwdMissingCancelMutatesNothing;
# this shows the user-visible half, which no unit test can look at.
#
# Cancel is also the default on every other dismissal path — esc, and simply
# never answering — so this frame is what all of them land on.
#
# The tape drives the "2" quick key rather than ↓+Enter: it is one keystroke,
# so nothing intermediate can be caught mid-flight in the recorded GIF, and it
# exercises the numbered leader the prompt advertises.
#
# Reuses the roster-cwd-missing scenario (only the driving keys differ), the
# same way roster-delete-lands reuses roster-delete-confirm's.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus the two PNGs under vhs/out/.

Output vhs/out/roster-cwd-missing-cancel.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario roster-cwd-missing"
Enter
Sleep 2s
Enter
Sleep 1s
Show

Screenshot vhs/out/roster-cwd-missing-cancel-before.png
Sleep 500ms
Type "2"
Sleep 1s
Screenshot vhs/out/roster-cwd-missing-cancel.png
Sleep 100ms
Ctrl+C
Sleep 200ms
Ctrl+C
