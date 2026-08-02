package uicopy

// yolo.go holds the /yolo guardrail toggle's copy: its two argument
// rejections, its two failure prefixes, and the two status notes that say what
// the toggle actually reached.

// YoloUnknownArgumentPrefix rejects a /yolo argument by name rather than
// silently toggling on it — on a guardrail switch, guessing would be the wrong
// direction half the time. The offending argument follows, quoted.
const YoloUnknownArgumentPrefix = "/yolo takes on or off — got "

// YoloTooManyArgumentsPrefix rejects a /yolo call carrying several arguments.
// The count follows.
const YoloTooManyArgumentsPrefix = "/yolo takes at most one argument — got "

// YoloConfigLoadErrorPrefix labels a config read that failed, which aborts the
// toggle rather than writing a zero-value config back.
const YoloConfigLoadErrorPrefix = "couldn't load config: "

// YoloSaveErrorPrefix labels a config write that failed.
const YoloSaveErrorPrefix = "couldn't save permission mode: "

// The two notes, as constants so the width test can measure them and /help can
// stay silent about wording. Both name NEW sessions and neither claims anything
// about the running one — the SDK fixes a session's guard at construction and
// carries no contract op for changing it. Both fit inside the 80-column floor
// the golden tests pin, with room to spare: the status line is truncated to the
// terminal width (App.render), and a caveat that gets cut off leaves the
// unqualified overclaim behind.
const (
	YoloOnNote  = "Guardrails OFF (yolo) for NEW sessions; running sessions keep theirs."
	YoloOffNote = "Guardrails ON (ask) for new sessions; running sessions keep theirs."
)
