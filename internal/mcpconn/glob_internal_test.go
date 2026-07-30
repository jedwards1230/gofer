package mcpconn

import "testing"

func TestSpecifierMatch(t *testing.T) {
	cases := []struct {
		spec, target string
		want         bool
	}{
		{"", "anything", true},
		{"*", "anything", true},
		{"read_*", "read_page", true},
		{"read_*", "write_page", false},
		{"admin:*", "admin", true},
		{"admin:*", "admin_delete", true},
		{"admin:*", "administrator", true}, // prefix match, not a glob boundary
		{"admin:*", "read_page", false},
	}
	for _, c := range cases {
		if got := specifierMatch(c.spec, c.target); got != c.want {
			t.Errorf("specifierMatch(%q, %q) = %v, want %v", c.spec, c.target, got, c.want)
		}
	}
}

func TestAllowedTool(t *testing.T) {
	cases := []struct {
		name        string
		allow, deny []string
		tool        string
		want        bool
	}{
		{"empty allow means everything", nil, nil, "anything", true},
		{"allow filters to match", []string{"read_*"}, nil, "write_page", false},
		{"allow admits match", []string{"read_*"}, nil, "read_page", true},
		{"deny always wins", []string{"*"}, []string{"delete_*"}, "delete_page", false},
		{"deny with no allow still applies", nil, []string{"delete_*"}, "delete_page", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := allowedTool(c.allow, c.deny, c.tool); got != c.want {
				t.Errorf("allowedTool(%v, %v, %q) = %v, want %v", c.allow, c.deny, c.tool, got, c.want)
			}
		})
	}
}
