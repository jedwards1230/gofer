# gofer overview/roster TUI — the flat view's cwd group headers contracting
# $HOME to "~" (gofer#337), captured as a before/after PAIR.
#
# Every other roster/panel scene's fixture Cwd is the LITERAL string
# "~/orchestration" — it never starts with a real absolute $HOME on any
# machine, so it never touches the contraction code at all and can't
# demonstrate this change. overviewCwdHomeScene instead seeds absolute
# working directories, and this tape runs the harness TWICE with a
# different HOME each time so HOME — not a code difference — is the only
# variable between the two frames:
#
#   - before: HOME=/Users/nobody, which matches none of the seeded
#     paths, so every header renders its full absolute path — the
#     pre-#337 appearance every path had, unconditionally.
#   - after: HOME=/Users/justin. Three headers contract to "~"-relative
#     spellings; the fourth, /Users/justinother/notes — a SIBLING
#     directory that only shares "/Users/justin" as a text prefix — is
#     still on screen unchanged, in the SAME frame, proving the
#     path-boundary check (not a naive strings.HasPrefix) is what's
#     actually running.
#
# Run via scripts/tui-vhs.sh (it prebuilds vhs/.bin/harness first). Outputs
# a GIF plus two PNGs (before/after) under vhs/out/.

Output vhs/out/roster-cwd-home.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20

Hide
Type "HOME=/Users/nobody ./vhs/.bin/harness -scenario roster-cwd-home"
Enter
Show

Sleep 2s
Screenshot vhs/out/roster-cwd-home-before.png
Sleep 100ms
Ctrl+C

Hide
Sleep 300ms
Type "HOME=/Users/justin ./vhs/.bin/harness -scenario roster-cwd-home"
Enter
Show

Sleep 2s
Screenshot vhs/out/roster-cwd-home.png
Sleep 100ms
Ctrl+C
