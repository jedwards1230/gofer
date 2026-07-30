package main

import (
	"fmt"
	"io"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/prompt"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// resolveSystemPrompt loads rootDir's config and composes the system prompt a
// new/resumed session rooted at cwd starts with (see internal/prompt.Compose
// for the resolution and composition rules) — the one place run/resume/exec
// build a runner.Options.System from, replacing the old defaultSystemPrompt
// string constant. A missing prompt file warns to stderr rather than failing
// the command, unless the loaded config's prompt.missing_file_is_error opts
// into the stricter behavior.
func resolveSystemPrompt(rootDir, cwd string, stderr io.Writer) (prompt.Composed, error) {
	cfg, err := config.Load(config.DefaultPath(rootDir))
	if err != nil {
		return prompt.Composed{}, err
	}
	// Compose returns warnings independently of err: a nil err does NOT mean
	// there were none. A missing optional file (the normal case for an absent
	// AGENTS.md) is a warning, not a failure — surface it and carry on. Only a
	// non-nil err aborts, which is what prompt.missing_file_is_error promotes a
	// missing file into.
	composed, warnings, err := prompt.Compose(cfg.Prompt, cwd, rootDir)
	if err != nil {
		return prompt.Composed{}, err
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(stderr, "gofer: prompt: %s\n", w)
	}
	return composed, nil
}

// recordPromptProvenance persists composed's provenance beside the session's
// journal (see supervisor.RecordPrompt) so a resumed session's actual prompt
// stays greppable on disk rather than only ever re-derivable from current
// config. A write failure is reported to stderr as a warning, never as a
// reason to fail an already-running session.
func recordPromptProvenance(id, journalPath string, composed prompt.Composed, stderr io.Writer) {
	err := supervisor.RecordPrompt(id, journalPath, supervisor.PromptProvenance{
		Files:  composed.Sources,
		SHA256: composed.SHA256,
		Bytes:  len(composed.Text),
	}, composed.Text)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gofer: warning: record prompt provenance: %v\n", err)
	}
}
