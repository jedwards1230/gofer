package prompt

import _ "embed"

// builtinSystemAsset is the "builtin:" name [config.DefaultPromptAsset]
// spells ("builtin:system.md") and the only builtin asset gofer ships today.
const builtinSystemAsset = "system.md"

// builtinSystemMD is gofer's default baseline system prompt, compiled into
// the binary so a fresh install runs with no filesystem dependency — the
// same argument cmd/gofer/demo.go's embedded manifest makes, gofer's only
// other use of the embed directive. Its content is unchanged from the Go
// string constant this package replaces (cmd/gofer's old
// defaultSystemPrompt): this move is code-to-data, not a prompt rewrite.
//
//go:embed system.md
var builtinSystemMD string

// builtinAsset resolves a "builtin:<name>" entry's name to its embedded
// content. ok is false for any name gofer has not shipped an asset for — a
// config typo (e.g. "builtin:sytem.md"), not an I/O failure, so [Compose]
// treats it exactly like every other unresolvable source.
func builtinAsset(name string) (string, bool) {
	if name == builtinSystemAsset {
		return builtinSystemMD, true
	}
	return "", false
}
