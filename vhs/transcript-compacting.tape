# gofer TUI — compaction in progress: the transient `⋯ compacting context… (Ns)`
# indicator at the transcript tail while an explicit /compact runs.
#
# This is the state that previously had NO honest signal at all. Compaction is
# a whole summarizer model call over the folded history and streams nothing
# while it runs, so for a minute or more the TUI looked frozen; the old
# affordance was a status note under the input that nothing ever cleared, so a
# FINISHED compaction read as permanently in progress. The Ascii goldens pin
# the indicator's text, but the muted styling that makes it read as subdued
# chrome rather than a message is invisible to them — that is this tape's job.
#
# The scene needs a Supervisor whose Compact BLOCKS (harness:
# vhsSupervisor.compactHold). Against the instant-returning Compact every other
# scene uses, the indicator would appear and vanish inside a single frame with
# nothing left to photograph — the state under capture would not exist.
#
# TIMING IS LOAD-BEARING, and deliberately mid-second. The counter truncates,
# so it reads "2s" for the whole of the third second; screenshotting ~2.5s
# after dispatch lands in the MIDDLE of that bucket rather than on its edge.
# The tracked snapshot baseline re-renders on every push to main
# (vhs-baseline.yml), so a frame captured on a bucket boundary would flip
# between two values and churn the baseline forever — see gofer#297.
#
# The transcript ABOVE the indicator is empty, which is a property of every
# canned-App scene in this harness (vhsSupervisor.Subscribe hands back a live
# but silent subscription — see its doc). It is worth naming because compaction
# is precisely about a LONG history being replaced, so an empty transcript
# undersells the situation even though the indicator itself is exactly right.
# Populating it means retaining a scripted backlog on the broker for the App to
# replay on attach (event.WithReplay, must-deliver tier only) — deliberately not
# done here, because that reintroduces publish-vs-subscribe ordering into a tape
# whose whole point is a frame that renders identically on every run.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus a PNG under vhs/out/. The GIF is where the counter is seen actually
# ADVANCING, which is the part a still frame cannot show.

Output vhs/out/transcript-compacting.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario transcript-compacting"
Enter
Show

# Attach to the selected session: the indicator renders at the ATTACH
# transcript's tail (App.attachModel), not on the roster.
Sleep 2s
Enter
Sleep 1s

# Dispatch the compaction. Compact blocks in the harness, so everything from
# here on is the in-flight state.
Type "/compact"
Enter

# Mid-bucket capture — see the timing note above.
Sleep 2500ms
Screenshot vhs/out/transcript-compacting.png

# Hold a few more seconds so the GIF shows the counter advancing.
Sleep 3s
Ctrl+C
