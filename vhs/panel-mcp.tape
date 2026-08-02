# gofer TUI — the /mcp command-panel tab (jedwards1230/gofer#303), captured as
# a BEFORE/AFTER PAIR because a single frame cannot show the thing that matters.
#
# Frame 1 (panel-mcp.png) is a backend that ANSWERED: four servers, one per
# state the tab has a distinct word for — connected, not connected, an
# unrecognized transport, disabled — plus the total federated tool count and
# the configured index/resident split.
#
# Frame 2 (panel-mcp-unknown.png) is the same tab attached to a backend that
# CANNOT answer: a `gofer daemon --workers` router (whose sessions live in
# worker processes, so the router owns no MCP manager at all) or a daemon older
# than gofer/capabilities. It renders an explicit UNKNOWN block.
#
# The pair IS the review: the failure this feature exists to prevent is an
# unanswered panel that reads as "no MCP servers configured", and the only way
# to see that the two are unmistakable is to look at them side by side. The
# Ascii goldens pin both texts but render termenv.Ascii — they cannot show that
# the states are also colour-distinguishable at a glance.
#
# Both scenes are in-process closures: no network IO, nothing time-varying, so
# the frames are stable for the tracked baseline (gofer#297).
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF plus two PNGs under vhs/out/.

Output vhs/out/panel-mcp.gif

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
Type "/mcp"
Enter
Sleep 1s
Screenshot vhs/out/panel-mcp.png
Sleep 100ms
Ctrl+C
Ctrl+C

# The UNKNOWN half, in the same recording so the two frames sit adjacent in the
# GIF rather than in two unrelated artifacts.
Hide
Sleep 500ms
Type "./vhs/.bin/harness -scenario panel-capabilities-unknown"
Enter
Show

Sleep 1s
Type "/mcp"
Enter
Sleep 1s
Screenshot vhs/out/panel-mcp-unknown.png
Sleep 100ms
Ctrl+C
Ctrl+C
