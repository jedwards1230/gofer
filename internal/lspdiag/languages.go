package lspdiag

import (
	"path/filepath"
	"strings"
)

// extToLanguage maps a lowercased file extension to the LSP languageId keys
// [agent-sdk-go/lsp.DefaultRegistry] registers servers under. This table is
// gofer's own policy — the SDK's registry is language-keyed, not
// extension-keyed, and deliberately leaves "which file extensions map to
// which language" to the consuming application (see agent-sdk-go's lsp
// package doc: the ~370-server nvim-lspconfig-shaped dataset, and everything
// that would resolve an extension against it, is explicitly future,
// consumer-side work).
var extToLanguage = map[string]string{
	".go":  "go",
	".ts":  "typescript",
	".tsx": "typescript",
	".js":  "javascript",
	".jsx": "javascript",
	".py":  "python",
	".rs":  "rust",
	".c":   "c",
	".h":   "c",
	".cpp": "cpp",
	".cc":  "cpp",
	".cxx": "cpp",
	".hpp": "cpp",
}

// languageForPath returns the LSP languageId for path's extension, and false
// for an extension with no entry in [extToLanguage] (a file lspdiag has
// nothing registered to diagnose).
func languageForPath(path string) (string, bool) {
	lang, ok := extToLanguage[strings.ToLower(filepath.Ext(path))]
	return lang, ok
}

// fileURI renders an absolute filesystem path as a file:// URI. It handles
// the plain, unescaped paths a coding-tool workspace normally has; a path
// containing characters that need percent-encoding is not specially handled,
// matching the simplicity the SDK's own lsp package keeps (Client.DidOpen
// takes a caller-supplied URI verbatim).
func fileURI(absPath string) string {
	return "file://" + filepath.ToSlash(absPath)
}
