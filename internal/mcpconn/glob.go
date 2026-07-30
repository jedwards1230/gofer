package mcpconn

import (
	"path"
	"strings"
)

// specifierMatch implements the SAME glob grammar [config.MCPServer]'s
// Allow/Deny doc promises ("the same glob grammar Rule.Specifier uses"):
// ""/"*" matches everything, a "prefix:*" spec matches by prefix (the target
// itself or anything starting with prefix), otherwise it's a path.Match
// glob. Duplicated from the SDK permission package's unexported
// specMatches — that grammar is exactly what config.MCPServer's doc commits
// to, and the SDK exports no reusable matcher for a non-permission caller to
// share it with.
func specifierMatch(spec, target string) bool {
	if spec == "" || spec == "*" {
		return true
	}
	if strings.HasSuffix(spec, ":*") {
		prefix := strings.TrimSuffix(spec, ":*")
		return target == prefix || strings.HasPrefix(target, prefix)
	}
	ok, err := path.Match(spec, target)
	return err == nil && ok
}

// anyMatch reports whether target matches any of patterns.
func anyMatch(patterns []string, target string) bool {
	for _, p := range patterns {
		if specifierMatch(p, target) {
			return true
		}
	}
	return false
}

// allowedTool reports whether a server's tool named name (its ORIGINAL,
// server-local name — see config.MCPServer's doc) should be projected: an
// empty Allow means every tool, otherwise name must match at least one Allow
// pattern; a Deny match always wins regardless of Allow.
func allowedTool(allow, deny []string, name string) bool {
	if len(allow) > 0 && !anyMatch(allow, name) {
		return false
	}
	return !anyMatch(deny, name)
}
