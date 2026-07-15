#!/usr/bin/env bash
# Capture the gofer attach TUI to GIF/PNG with charmbracelet VHS.
#
# On-demand dev tooling — NOT a CI gate. VHS shows real rendered frames (colors,
# spacing, glyphs) the Ascii golden tests can't, which is how we catch visual
# regressions like the #61 color scatter. The golden tests remain the
# authoritative assertion; this only complements them.
#
# Usage: scripts/tui-vhs.sh [slug...]   (no arg = all tapes; slugs match vhs/*.tape)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if ! command -v vhs >/dev/null 2>&1; then
	cat >&2 <<'EOF'
vhs not found. Install charmbracelet VHS to capture the TUI:

  go install github.com/charmbracelet/vhs@latest   # needs ttyd + ffmpeg on PATH
  # or: brew install vhs                            # pulls ttyd + ffmpeg too

Then re-run: scripts/tui-vhs.sh
See docs/TUI.md ("Visual capture with VHS") for details.
EOF
	exit 127
fi

mkdir -p vhs/.bin vhs/out

echo "building harness -> vhs/.bin/harness"
go build -o vhs/.bin/harness ./vhs/harness

tapes=("$@")
if [ ${#tapes[@]} -eq 0 ]; then
	tapes=(transcript-tool-call transcript-approval roster-overview panel-status-overview panel-status panel-config panel-model panel-model-empty)
fi

for name in "${tapes[@]}"; do
	tape="vhs/${name}.tape"
	if [ ! -f "$tape" ]; then
		echo "no such tape: $tape (see vhs/*.tape for the available slugs)" >&2
		exit 2
	fi
	echo "vhs $tape"
	vhs "$tape"
done

echo "done -> vhs/out/"
ls -1 vhs/out/
