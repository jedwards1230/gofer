# gofer TUI — the /skills command-panel tab (jedwards1230/gofer#303), captured
# as a BEFORE/AFTER PAIR for the same reason panel-mcp.tape is.
#
# Frame 1 (panel-skills.png) is a backend that ANSWERED: three loaded skills
# (one disabled, one whose description was truncated to the configured budget),
# a SHADOWED duplicate naming the losing file, a size-skipped candidate, and
# skillset.Summarize's operator line — which reaches a human here for the first
# time since it was written.
#
# Frame 2 (panel-skills-unknown.png) is the same tab against a backend that
# cannot answer, rendering the explicit UNKNOWN block.
#
# What only a colour render can show: the four registers on screen at once
# (ordinary loaded rows, a muted disabled row, a yellow shadowed row, a red
# skipped row) have to read as one coherent list rather than a scatter — and
# the Ascii goldens are blind to every one of those distinctions.
#
# Note what is deliberately absent from the loaded rows: a source path. The SDK
# skill index records none, and guessing the winner by re-walking the discovery
# directories would go wrong exactly when a first-directory candidate failed to
# load for an unrelated reason. Only the shadowed/skipped rows carry paths,
# because those are the ones the loader actually named.
#
# Both scenes are in-process closures: no network IO, nothing time-varying.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus two PNGs under vhs/out/.

Output vhs/out/panel-skills.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario panel-capabilities"
Enter
Show

Sleep 1s
Type "/skills"
Enter
Sleep 1s
Screenshot vhs/out/panel-skills.png
Sleep 100ms
Ctrl+C
Ctrl+C

# The UNKNOWN half, in the same recording so the two frames sit adjacent.
Hide
Sleep 500ms
Type "./vhs/.bin/harness -scenario panel-capabilities-unknown"
Enter
Show

Sleep 1s
Type "/skills"
Enter
Sleep 1s
Screenshot vhs/out/panel-skills-unknown.png
Sleep 100ms
Ctrl+C
Ctrl+C
