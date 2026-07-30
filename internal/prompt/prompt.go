// Package prompt composes a session's system prompt from
// [config.Prompt]'s ordered file list — the replacement for cmd/gofer's old
// defaultSystemPrompt string constant (see docs/PRD.md's "no hardcoded
// prompts" constraint). It is a leaf over internal/config: no bubbletea, no
// runner, no network — cmd/gofer's run/resume/exec call [Compose] once each,
// before they build their runner.Options, and internal/supervisor journals
// the result beside a session's journal (see [supervisor.RecordPrompt]) so a
// later addition (a tool-index hint, workstream 4) has exactly one place to
// extend rather than three call sites to revisit.
//
// # Resolution
//
// Each [config.Prompt.Files] entry resolves independently, per the rules
// documented on [config.Prompt] and implemented in resolveEntry:
//
//   - "builtin:<name>" loads an embedded asset — the only non-path form.
//   - An absolute path is read verbatim.
//   - A "~/…" path expands against the user's home directory.
//   - Any other relative path is resolved against the session's cwd first,
//     then the store root; the first hit wins.
//
// # Composition
//
// Resolved sources are read in list order and joined by a blank line, each
// trimmed of leading/trailing whitespace first — so a markdown file's
// trailing newline never leaks a stray blank line into the join, and the
// single-file (default) case reproduces the old string constant exactly. An
// entry whose resolved identity repeats an earlier one in the list is
// skipped before it is even read — first-wins de-dup, not a re-read of the
// same content. A file that can't be found or read is skipped and reported
// as a [Warning], unless [config.Prompt.MissingFileIsError] turns it into a
// hard failure instead — an absent AGENTS.md is the normal case in most
// repos and must not silently blind a session, but a misconfigured required
// file should.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jedwards1230/gofer/internal/config"
)

// Composed is [Compose]'s result: the joined system prompt text, the
// resolved sources that actually contributed to it (in composed order, post
// de-dup — a builtin source reads back as "builtin:<name>", a file source as
// its resolved absolute path), and a hex SHA-256 digest of Text.
//
// Sources and SHA256 are what [supervisor.RecordPrompt] journals into a
// session's `.meta.json` sidecar, so which files actually shaped a session's
// prompt — and whether that content later changed — stays greppable on disk
// (see CLAUDE.md's "visible artifacts over hidden state").
type Composed struct {
	Text    string
	Sources []string
	SHA256  string
}

// Warning is one [config.Prompt.Files] entry [Compose] could not resolve or
// read. It is an error value so a caller can log it, but Compose does not
// return it as ITS error — a bad file is skipped, not fatal, unless
// [config.Prompt.MissingFileIsError] says otherwise.
type Warning struct {
	Entry string
	Err   error
}

func (w Warning) Error() string { return w.Entry + ": " + w.Err.Error() }

func (w Warning) Unwrap() error { return w.Err }

// Compose resolves p.Sources() against cwd (a session's working directory,
// tried first for a bare relative entry) and storeRoot (tried second), reads
// and joins each resolved source, and returns the result.
//
// Compose's own error return is reserved for [config.Prompt.MissingFileIsError]:
// with it unset (the default), an unresolvable source degrades to a
// [Warning] in the second return and composition continues with whatever
// else resolved — even an empty result (Text == "") is a valid Composed, not
// an error, if every source failed to resolve and MissingFileIsError is
// false.
func Compose(p config.Prompt, cwd, storeRoot string) (Composed, []Warning, error) {
	limit := p.FileLimitBytes()
	seen := make(map[string]bool)
	var parts, contributed []string
	var warnings []Warning

	for _, entry := range p.Sources() {
		r := resolveEntry(entry, cwd, storeRoot)
		if seen[r.key] {
			continue
		}
		seen[r.key] = true

		text, err := r.read(limit)
		if err != nil {
			if p.MissingFileIsError {
				return Composed{}, nil, fmt.Errorf("prompt: %s: %w", entry, err)
			}
			warnings = append(warnings, Warning{Entry: entry, Err: err})
			continue
		}
		parts = append(parts, strings.TrimSpace(text))
		contributed = append(contributed, r.key)
	}

	text := strings.Join(parts, "\n\n")
	sum := sha256.Sum256([]byte(text))
	return Composed{
		Text:    text,
		Sources: contributed,
		SHA256:  hex.EncodeToString(sum[:]),
	}, warnings, nil
}
