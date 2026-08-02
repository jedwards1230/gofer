package uicopy

import "fmt"

// Operator-facing copy for the `!` / `!!` shell escape
// (internal/tui/shell.go).
//
// Only part of that file's strings are copy. The fold header, the no-output
// marker, the `[exit %d]` / `[output truncated]` markers, and a run's note are
// written into the MODEL's context and stay beside the prompt composer that
// emits them — see the note above that group in internal/tui/shell.go.

// ShellNothingToRun is the refusal for a bare `!` / `!!`: handing an empty
// string to `sh -c` would report success for a command the user never
// finished typing.
const ShellNothingToRun = "nothing to run — type a command after !"

// The one-line disposition a finished run wears: whether its output is the
// model's to see. DISPLAY only — internal/tui's composePrompt is what
// actually includes or excludes the bytes.
const (
	ShellSentWithNextMessage = "sent with your next message"
	ShellNotSentToAgent      = "not sent to the agent"
)

// ShellRunExited and ShellRunOK are the status-line acknowledgements a
// finished run posts on screens with no transcript to render it into (the
// overview, peek), so a `!` typed at the dispatch bar still says that it ran
// and where its output went. disposition is one of the two constants above,
// already prefixed with its separator by the caller.
func ShellRunExited(line string, code int, disposition string) string {
	return fmt.Sprintf("%s exited %d%s", line, code, disposition)
}

// ShellRunOK acknowledges a clean run.
func ShellRunOK(line, disposition string) string { return "ran " + line + disposition }
