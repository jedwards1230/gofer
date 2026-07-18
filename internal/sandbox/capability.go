package sandbox

// containableTools names the builtin tools a sandbox backend can hold once
// its runtime is available. bash is the one that matters — it is the only
// builtin that shells out to the host, and containing it is the whole point
// of this package. The file tools (read/write/edit/ls/glob/grep) already
// confine themselves to the workdir they were constructed with, so they are
// containable too — a backend gains nothing by asking a human about them.
// update_plan is mutation-free (it validates and records the model's task
// plan; no fs/network/exec), so it is strictly safer than the file tools and
// belongs here too — otherwise every plan revision would escalate to a human
// "ask" for no benefit.
var containableTools = map[string]bool{
	"bash":        true,
	"read":        true,
	"edit":        true,
	"write":       true,
	"ls":          true,
	"glob":        true,
	"grep":        true,
	"update_plan": true,
}

// containableTool reports whether name is a tool call a sandbox backend can
// hold. It is the shared predicate both OS backends' CanContain builds on.
func containableTool(name string) bool { return containableTools[name] }
