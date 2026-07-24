package tui

// link.go makes the URLs in rendered markdown clickable. Two composing layers:
//
//   - OSC 8 hyperlinks. [linkifyURLs] wraps each visible http(s) URL in the
//     glamour-rendered output with an OSC 8 escape (`\x1b]8;;URL\x1b\\…\x1b]8;;
//     \x1b\\`), so terminals that support OSC 8 make the link natively clickable.
//     Emitted ONLY on a real color profile (markdown.go gates on the Ascii golden
//     profile), and the sequence is zero-width and ANSI-stripped, so it never
//     changes a row's width, a golden file, or the selection/highlight math (the
//     charmbracelet/x/ansi Strip/StringWidth/Cut helpers all treat OSC 8 as
//     zero-width and preserve it across a cut).
//   - App-side open. Because the TUI captures mouse events (cell-motion mode),
//     a click can't always reach the terminal's own OSC 8 handler, so a
//     modifier+click on a link span opens it via the platform opener
//     ([openURLCmd] → open / xdg-open / rundll32 by runtime.GOOS). [linkAt]
//     recovers the URL under a cell straight from the OSC 8 the render already
//     emitted, so there is one source of link truth. A plain (unmodified) click
//     or drag stays pure selection, so link-open composes with click/word
//     selection (mouse.go).

import (
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// httpURLPattern matches a bare http(s) URL up to the first whitespace or ANSI
// escape. glamour prints a link's href as an isolated, style-wrapped token, so
// the run stops at the trailing SGR reset (an ESC) rather than swallowing it.
var httpURLPattern = regexp.MustCompile(`https?://[^\s\x1b]+`)

// linkifyURLs wraps every visible http(s) URL in line with an OSC 8 hyperlink
// pointing at itself, so a terminal can make the printed URL clickable. The
// visible text is unchanged (the URL still shows); only the zero-width OSC 8
// framing is added. Idempotent enough for this pipeline: glamour never emits
// OSC 8 itself and linkify runs once per rendered row.
func linkifyURLs(line string) string {
	return httpURLPattern.ReplaceAllStringFunc(line, func(u string) string {
		return "\x1b]8;;" + u + "\x1b\\" + u + "\x1b]8;;\x1b\\"
	})
}

// linkAt returns the OSC 8 hyperlink URL covering cell column x on row y of the
// composed frame, if any. It walks the row tracking the currently-open OSC 8
// target and the visible cell position, skipping every other escape (CSI/SGR,
// other OSC) as zero-width — so the column it matches against x is the same cell
// column the mouse reports and the selection math uses. ok is false when the
// cell carries no hyperlink.
func linkAt(frame string, x, y int) (url string, ok bool) {
	lines := strings.Split(frame, "\n")
	if y < 0 || y >= len(lines) {
		return "", false
	}
	line := lines[y]
	cur := ""
	col := 0
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) {
			switch line[i+1] {
			case '[': // CSI: ESC [ … final byte 0x40–0x7e
				j := i + 2
				for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
					j++
				}
				i = j + 1
				continue
			case ']': // OSC: ESC ] … ST (ESC \) or BEL
				body, next := readOSC(line, i)
				if t, isLink := strings.CutPrefix(body, "8;;"); isLink {
					cur = t // "" closes the hyperlink
				}
				i = next
				continue
			default: // other 2-byte escape
				i += 2
				continue
			}
		}
		r, sz := utf8.DecodeRuneInString(line[i:])
		w := ansi.StringWidth(string(r))
		if x >= col && x < col+w {
			return cur, cur != ""
		}
		col += w
		i += sz
	}
	return "", false
}

// readOSC returns an OSC sequence's body (the bytes between "ESC ]" and its
// terminator) and the index just past the terminator, given the ESC at start.
// It accepts both the ST (ESC \) and BEL terminators. An unterminated OSC
// consumes the rest of the line.
func readOSC(s string, start int) (body string, next int) {
	i := start + 2 // past "ESC ]"
	for i < len(s) {
		if s[i] == 0x07 { // BEL
			return s[start+2 : i], i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' { // ST
			return s[start+2 : i], i + 2
		}
		i++
	}
	return s[start+2:], len(s)
}

// openURLCmd returns a command that opens url in the user's default handler via
// the platform opener, best-effort and non-blocking. It opens only http(s) URLs
// (the only scheme linkifyURLs ever wraps), so a click can never launch an
// arbitrary program. The exec runs inside the returned [tea.Cmd], which the
// bubbletea runtime invokes — a unit test that only builds the command never
// shells out.
func openURLCmd(url string) tea.Cmd {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil
	}
	return func() tea.Msg {
		if args := openURLArgs(runtime.GOOS, url); len(args) > 0 {
			_ = exec.Command(args[0], args[1:]...).Start()
		}
		return nil
	}
}

// openURLArgs returns the argv that opens url on the given GOOS: `open` on
// macOS, `rundll32` on Windows, and `xdg-open` as the default everywhere else
// (Linux/BSD). A default rather than a single hardcoded OS, so the opener works
// on any platform gofer runs on.
func openURLArgs(goos, url string) []string {
	switch goos {
	case "darwin":
		return []string{"open", url}
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", url}
	default:
		return []string{"xdg-open", url}
	}
}
