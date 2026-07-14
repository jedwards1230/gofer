package sandbox

// containableTools names the builtin tools a sandbox backend can hold once
// its runtime is available. bash is the one that matters — it is the only
// builtin that shells out to the host, and containing it is the whole point
// of this package. The file tools (read/write/edit/ls/glob/grep) already
// confine themselves to the workdir they were constructed with, so they are
// containable too — a backend gains nothing by asking a human about them.
var containableTools = map[string]bool{
	"bash":  true,
	"read":  true,
	"edit":  true,
	"write": true,
	"ls":    true,
	"glob":  true,
	"grep":  true,
}

// containableTool reports whether name is a tool call a sandbox backend can
// hold. It is the shared predicate both OS backends' CanContain builds on.
func containableTool(name string) bool { return containableTools[name] }
