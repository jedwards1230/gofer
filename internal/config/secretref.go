package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// SecretRef is a reference to a secret value, never the value itself:
// "env:VAR" names an environment variable, "file:/path" names a file whose
// trimmed contents are the secret. Every credential field in this package
// (MCP server env/headers/auth, a search provider's API key) is a SecretRef,
// never a plain string — see [Config.validate], which rejects a non-empty
// SecretRef with no recognized scheme so pasting a token straight into
// config.json is a load error naming the mistake, not a committed
// credential.
//
// [SecretRef.Resolve] runs at USE time, deliberately not at config load
// time: a credential a session never actually calls (a disabled MCP server,
// an unselected search provider) must not break every OTHER section just
// because its env var happens to be unset on this host.
//
// There is deliberately no exec: scheme — config load must never spawn a
// process just to read a value. ("op://" may join this type in a later
// milestone; it is not implemented here.)
type SecretRef string

// Resolve reads the secret r refers to. An empty ref resolves to ("", nil) —
// "not configured" is valid for every optional SecretRef field in this
// package. Errors name the scheme and the ref itself, NEVER the resolved
// value, so a resolve failure is safe to log or surface on a status line
// without risking a credential leak.
func (r SecretRef) Resolve() (string, error) {
	s := string(r)
	switch {
	case s == "":
		return "", nil
	case strings.HasPrefix(s, "env:"):
		name := strings.TrimPrefix(s, "env:")
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("secretref: env %q is not set", name)
		}
		return v, nil
	case strings.HasPrefix(s, "file:"):
		path := strings.TrimPrefix(s, "file:")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("secretref: read file %q: %w", path, err)
		}
		return strings.TrimSpace(string(data)), nil
	default:
		return "", fmt.Errorf("secretref: unrecognized scheme in %q (want env:VAR or file:/path)", s)
	}
}

// hasRecognizedScheme reports whether r carries one of the two schemes
// [SecretRef.Resolve] understands. Used by [SecretRef.validate]; does not
// itself judge emptiness.
func (r SecretRef) hasRecognizedScheme() bool {
	s := string(r)
	return strings.HasPrefix(s, "env:") || strings.HasPrefix(s, "file:")
}

// validate rejects a non-empty SecretRef with no recognized scheme. An empty
// ref is valid: every SecretRef field in this package is optional, and
// "" means "not configured", not "inline secret with no scheme".
func (r SecretRef) validate() error {
	if r == "" || r.hasRecognizedScheme() {
		return nil
	}
	return errors.New("secrets are referenced, never inlined: use env:VAR or file:/path")
}
