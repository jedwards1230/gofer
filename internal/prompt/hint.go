package prompt

import "strings"

// AppendHint appends hint — e.g. the SDK's toolindex.Index.Hint(), gofer's
// tool-search index summary — to a composed system prompt as a trailing,
// blank-line-separated section. It is the single place this join happens: a
// caller that builds both a [Composed] prompt and a tool-index hint (today,
// internal/supervisor's sessionGuard/Create/Resume, when tools.schema_mode
// resolves to index) calls this rather than re-deriving its own join
// discipline, so the two stay formatted identically wherever they meet.
//
// An empty hint is a no-op, returning text unchanged — a preload-mode
// session, or an index with nothing to hint, never gets a stray trailing
// blank section. An empty text with a non-empty hint returns hint alone,
// mirroring [Compose]'s own "no stray leading blank line" discipline for a
// single contributing source.
func AppendHint(text, hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return text
	}
	if text == "" {
		return hint
	}
	return text + "\n\n" + hint
}
