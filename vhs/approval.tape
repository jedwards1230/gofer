# gofer attach TUI — a turn that ends in a pending permission request.
# Captures the inline approval prompt (commandeering the input line) and, above
# it, the softened error styling of a failed tool call (warn-accent header,
# dimmed result body).
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs a
# GIF of the whole turn plus a PNG of the pending-approval frame under vhs/out/.

Output vhs/out/approval.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "./vhs/.bin/harness -scenario approval"
Enter
Show

Sleep 3500ms
Screenshot vhs/out/approval.png
Sleep 500ms
Ctrl+C
