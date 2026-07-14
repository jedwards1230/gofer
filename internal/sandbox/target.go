package sandbox

import (
	"encoding/json"

	"github.com/jedwards1230/agent-sdk-go/loop"
)

// toolTarget decodes the specifier match string from a tool call's JSON input.
// It mirrors the SDK builtin tools' input schemas: bash carries {command}, the
// file tools carry {path} (glob/grep also accept {pattern}). Malformed or
// empty input yields "".
func toolTarget(call loop.ToolCall) string {
	if len(call.Input) == 0 {
		return ""
	}
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return ""
	}
	switch {
	case in.Command != "":
		return in.Command
	case in.Path != "":
		return in.Path
	case in.Pattern != "":
		return in.Pattern
	default:
		return ""
	}
}
