# gofer TUI — AUTOMATIC compaction becoming visible (gofer#300): the transient
# `⋯ compacting context… (Ns)` indicator appearing at the transcript tail with
# no slash command, no keystroke, and no client call in flight.
#
# This is a DIFFERENT state from transcript-compacting.tape, not a second angle
# on it, and the difference is the entire feature. That tape captures an
# indicator the user asked for: the tape types /compact, and the indicator lives
# exactly as long as the held call does. An automatic compaction has no call and
# no keystroke — the pump triggers it supervisor-side — so before
# session.compaction_started existed there was NOTHING for the client to hang an
# indicator on, and the transcript simply froze for a minute with no explanation.
# The user could not tell a compaction from a hang.
#
# The tape therefore captures a PAIR, and needs both halves. Frame 1 is the
# attached transcript with no indicator; frame 2 is the same transcript with the
# indicator present. One frame of an indicator can show that it exists, but it
# cannot show that it APPEARED ON ITS OWN — and "appeared with no user action"
# is the reviewable claim here. Compare the two frames and the untouched input
# line between them: nothing was typed.
#
# The scene publishes the start event on a TIMER (harness: autoCompactingApp,
# autoCompactDelay = 4s) rather than seeding it into the replay backlog. Seeding
# it would put the indicator on screen the instant the tape attaches, which
# destroys the "before" frame and with it the pair. The timer is also what makes
# this an honest mock: an automatic compaction genuinely arrives mid-session,
# unannounced.
#
# No terminal event is ever published, so the indicator persists to the end of
# the tape — the same reason transcript-compacting holds its Compact call. The
# clearing paths are covered by the unit tests
# (compact_indicator_internal_test.go), which is the right layer for them: a
# tape cannot assert that something is ABSENT for the right reason.
#
# TIMING IS LOAD-BEARING, and deliberately mid-second, for the same reason as
# transcript-compacting: the counter truncates, so it reads "2s" for the whole
# of the third second. The start is published ~4s after the harness launches,
# and the "after" screenshot lands ~2.5s later — the MIDDLE of the "2s" bucket,
# leaving ~500ms of slack on either side for process-startup jitter. A frame
# captured on a bucket boundary would flip between two values and churn the
# committed baseline forever, since vhs-baseline.yml re-renders every tape on
# each push to main (gofer#297).
#
# The transcript underneath is the same MOCKED two-turn conversation
# transcript-compacting uses (harness: compactableHistory) — compaction exists
# to replace a long context, so the frame has to look like a session with
# something worth compacting.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus two PNGs under vhs/out/. The GIF is where the indicator is seen
# actually ARRIVING unprompted, which is the part a still frame cannot show.

Output vhs/out/transcript-auto-compacting.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario transcript-auto-compacting"
Enter
Show

# Attach to the selected session: the indicator renders at the ATTACH
# transcript's tail (App.attachModel), not on the roster.
Sleep 2s
Enter

# BEFORE — attached, seeded history on screen, no compaction yet. The start
# event is still ~1s away.
Sleep 1s
Screenshot vhs/out/transcript-auto-compacting-before.png

# The start event lands here with no input at all. Nothing is typed for the
# rest of the tape.
Sleep 1s

# AFTER — mid-bucket capture of the indicator that appeared on its own.
Sleep 2500ms
Screenshot vhs/out/transcript-auto-compacting-after.png

# Hold so the GIF shows the counter advancing past the still frame.
Sleep 3s
Ctrl+C
