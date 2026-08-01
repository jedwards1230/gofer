# gofer TUI — failure-triggered compaction (jedwards1230/gofer#279): the
# notice → session.compacted → answer sequence, as a user actually reads it.
#
# The sequence is the feature. A context-overflow rejection generates nothing —
# no text, no tool call, no usage — so without the notice the transcript jumps
# from the user's prompt straight to a compaction block and an answer, which
# reads as the session silently skipping a beat. This frame is what "you can
# tell the difference" looks like: the prompt, then a line saying the window
# was exceeded and what is being done about it, then the compaction, then the
# retried turn's reply.
#
# The Ascii goldens pin the text of all three blocks. What they cannot see is
# that they are three DIFFERENT registers stacked together — a muted non-fatal
# error line, an accent-styled structural block, and ordinary assistant text.
# Whether that stack reads as a coherent explanation or as three unrelated
# interruptions is a colour-and-weight question, which is this tape's job.
#
# NO before/after pair here, deliberately, unlike roster-delete-confirm. That
# guidance exists for a state whose meaning is a CHANGE (a highlight shifting
# to signal a pending confirm); this state's meaning is a sequence, and the
# sequence is legible in a single frame. The tape is still sensitive to the
# feature: drop the notice emit and the frame visibly loses its explanation.
#
# NOTHING HERE IS TIME-VARYING. The whole sequence is seeded onto the broker's
# retained backlog (harness: overflowRecoveryHistory via vhsSupervisor.seed),
# so it is already on screen the moment the tape attaches — no counter to
# capture mid-bucket, no publish-vs-subscribe race, nothing for the tracked
# baseline to churn on when vhs-baseline.yml re-renders it (gofer#297). The
# only Sleeps are to let the TUI paint.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG under vhs/out/.

Output vhs/out/transcript-overflow-recovery.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario transcript-overflow-recovery"
Enter
Show

# Attach to the selected session: the sequence renders in the ATTACH
# transcript (App.attachModel), not on the roster.
Sleep 2s
Enter
Sleep 1500ms

Screenshot vhs/out/transcript-overflow-recovery.png

# Hold briefly so the GIF ends on the settled frame rather than mid-paint.
Sleep 2s
Ctrl+C
