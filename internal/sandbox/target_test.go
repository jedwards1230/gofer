package sandbox

import (
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/loop"
)

func TestToolTarget(t *testing.T) {
	tests := []struct {
		name string
		call loop.ToolCall
		want string
	}{
		{
			name: "bash command",
			call: loop.ToolCall{Name: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)},
			want: "echo hi",
		},
		{
			name: "read path",
			call: loop.ToolCall{Name: "read", Input: json.RawMessage(`{"path":"/tmp/f.go"}`)},
			want: "/tmp/f.go",
		},
		{
			name: "write path",
			call: loop.ToolCall{Name: "write", Input: json.RawMessage(`{"path":"/tmp/f.go","content":"x"}`)},
			want: "/tmp/f.go",
		},
		{
			name: "edit path",
			call: loop.ToolCall{Name: "edit", Input: json.RawMessage(`{"path":"/tmp/f.go"}`)},
			want: "/tmp/f.go",
		},
		{
			name: "ls path",
			call: loop.ToolCall{Name: "ls", Input: json.RawMessage(`{"path":"/tmp"}`)},
			want: "/tmp",
		},
		{
			name: "glob pattern",
			call: loop.ToolCall{Name: "glob", Input: json.RawMessage(`{"pattern":"**/*.go"}`)},
			want: "**/*.go",
		},
		{
			name: "grep pattern",
			call: loop.ToolCall{Name: "grep", Input: json.RawMessage(`{"pattern":"TODO"}`)},
			want: "TODO",
		},
		{
			name: "command takes precedence over path",
			call: loop.ToolCall{Name: "bash", Input: json.RawMessage(`{"command":"echo hi","path":"/tmp"}`)},
			want: "echo hi",
		},
		{
			name: "empty input",
			call: loop.ToolCall{Name: "bash", Input: nil},
			want: "",
		},
		{
			name: "malformed json",
			call: loop.ToolCall{Name: "bash", Input: json.RawMessage(`not json`)},
			want: "",
		},
		{
			name: "unrecognized fields",
			call: loop.ToolCall{Name: "other", Input: json.RawMessage(`{"foo":"bar"}`)},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToolTarget(tt.call); got != tt.want {
				t.Errorf("ToolTarget(%+v) = %q, want %q", tt.call, got, tt.want)
			}
		})
	}
}
