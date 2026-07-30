package lspdiag

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// diagnosableTools names the builtin tools whose successful result carries a
// file mutation worth diagnosing. It must track tool.Edit.Name() /
// tool.Write.Name() exactly; this package deliberately does not import the
// tool package just to reference two string constants, so a rename there is
// a silent no-op here rather than a build break — loop.ToolRegistry is a
// runtime lookup by name either way.
var diagnosableTools = map[string]bool{"edit": true, "write": true}

// Wrap decorates tools so that a successful edit/write call is followed by a
// bounded, best-effort LSP diagnostics round-trip for the file it changed
// (see [Manager.Diagnose]): non-empty diagnostics are appended to BOTH the
// tool's model-facing Content (so the model actually sees them on its next
// turn) and its Diagnostics slot (loop.ToolResult.Diagnostics →
// event.ToolCallFinished.Diagnostics, already wired end to end to every
// client — internal/wirestream, internal/daemonbridge, the JSONL journal).
// Every other tool, a failed edit/write (IsError or a non-nil error), and an
// edit/write that produced no diagnostics all pass through with the result
// completely unchanged. Specs() is untouched — the model sees the same tool
// surface either way.
//
// A nil mgr or !opts.Enabled returns tools completely unwrapped — the
// zero-cost opt-out path, matching [sandbox.WrapRegistry]'s own shape for an
// unavailable Container.
func Wrap(tools loop.ToolRegistry, mgr *Manager, root string, opts Options) loop.ToolRegistry {
	if mgr == nil || !opts.Enabled {
		return tools
	}
	return wrappedRegistry{base: tools, mgr: mgr, root: root, opts: opts.resolve()}
}

type wrappedRegistry struct {
	base loop.ToolRegistry
	mgr  *Manager
	root string
	opts Options
}

func (r wrappedRegistry) Get(name string) (loop.Tool, bool) {
	t, ok := r.base.Get(name)
	if !ok || !diagnosableTools[name] {
		return t, ok
	}
	return diagnosingTool{base: t, mgr: r.mgr, root: r.root, opts: r.opts}, true
}

func (r wrappedRegistry) Specs() []provider.ToolSpec { return r.base.Specs() }

// diagnosingTool wraps one diagnosable loop.Tool (edit or write).
type diagnosingTool struct {
	base loop.Tool
	mgr  *Manager
	root string
	opts Options
}

func (t diagnosingTool) Run(ctx context.Context, input json.RawMessage) (loop.ToolResult, error) {
	res, err := t.base.Run(ctx, input)
	if err != nil || res.IsError || len(res.Edits) == 0 {
		return res, err
	}
	// edit/write each populate exactly one FileEdit for their own single-file
	// change (see the SDK's tool/edit.go, tool/write.go, and the
	// loop.ToolResult.Edits doc); take the first defensively rather than
	// assuming len==1 forever.
	edit := res.Edits[0]
	path := resolvePath(t.root, edit.Path)

	lines := t.mgr.Diagnose(ctx, t.root, path, edit.NewText, t.opts)
	if len(lines) == 0 {
		return res, nil
	}
	res.Content += formatDiagnostics(edit.Path, lines)
	res.Diagnostics = append(res.Diagnostics, lines...)
	return res, nil
}

// resolvePath mirrors the SDK builtin tools' own root-relative resolution
// (agent-sdk-go tool/helpers.go resolvePath) — event.FileEdit.Path carries
// the path exactly as the model supplied it (see tool/edit.go, tool/write.go
// building FileChange.Path from the input verbatim), which may be relative
// to the session's cwd.
func resolvePath(root, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(root, p)
}

func formatDiagnostics(path string, lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n[lsp diagnostics: %s]\n", path)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
