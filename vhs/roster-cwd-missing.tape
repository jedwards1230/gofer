# gofer TUI — the three-way prompt that replaces the bare error when a
# session's RECORDED working directory has been deleted
# (jedwards1230/gofer#326).
#
# Captured as a BEFORE/AFTER PAIR, because the subject is a state CHANGE and a
# single frame cannot show one:
#
#   - before: the ordinary roster, sess-1 selected, nothing to suggest that
#     opening it will fail.
#   - after:  Enter, and ~200ms later the daemon's typed cwd-missing signal
#     lands and the prompt takes the bottom of the frame.
#
# Before this change that second frame was a red status line reading
# `session cwd "…" does not exist` — an error with no remedy on it, and the
# session left unopenable with nothing to do about it. The pair is what shows
# that: an unchanged roster next to the offer of three actual choices.
#
# What only COLOR can show, and the Ascii golden (app_cwd_missing_prompt.golden)
# therefore cannot:
#   - the "Session directory is gone" heading in Danger red, so the frame reads
#     as a failure at a glance rather than as an ordinary panel;
#   - the cwd-scoped-context warning in Warn yellow under the re-init row —
#     the load-bearing caveat of the whole prompt, which must not read as
#     ordinary body text;
#   - the accent caret on the focused choice.
# Every one of those ALSO changes text (the heading's words, the warning's
# words, the caret glyph), which is the rule for anything the goldens must
# still be able to assert.
#
# Reuses the canned roster and CommandEnv every panel-* scene renders — the
# roster-cwd-missing scenario differs ONLY in that its Supervisor raises the
# signal when sess-1 is subscribed, the same way transcript-compacting differs
# only by a blocking Compact. Keeping the session set identical is what stops a
# new scene from churning six other tapes' snapshots.
#
# Deterministic: the harness's clock is frozen (fixedNow), the missing path is a
# fixed constant, and the prompt shows no elapsed counter — nothing here can
# drift the committed key-frames (gofer#297).
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus the two PNGs under vhs/out/.

Output vhs/out/roster-cwd-missing.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario roster-cwd-missing"
Enter
Show

Sleep 2s
Screenshot vhs/out/roster-cwd-missing-before.png
Sleep 500ms
Enter
Sleep 1s
Screenshot vhs/out/roster-cwd-missing.png
Sleep 100ms
Ctrl+C
Sleep 200ms
Ctrl+C
