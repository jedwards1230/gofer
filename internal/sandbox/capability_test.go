package sandbox

import "testing"

func TestContainableTool(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"bash", true},
		{"read", true},
		{"edit", true},
		{"write", true},
		{"ls", true},
		{"glob", true},
		{"grep", true},
		{"unknown_tool", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containableTool(tt.name); got != tt.want {
				t.Errorf("containableTool(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
