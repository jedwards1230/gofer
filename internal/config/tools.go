package config

// ToolSchemaMode is the resolved form of [Tools.SchemaMode]'s free-text
// config value.
type ToolSchemaMode string

const (
	// ToolSchemaModePreload puts every candidate tool's full schema in
	// context up front. It is the fail-safe default; see [Tools.Schemas].
	ToolSchemaModePreload ToolSchemaMode = "preload"
	// ToolSchemaModeIndex puts only a name+one-line-description index in
	// context; a full schema is fetched on demand via a tool_search tool
	// (M7 workstream 4, not implemented by this PR). Opt-in only.
	ToolSchemaModeIndex ToolSchemaMode = "index"
)

// DefaultResidentTools is [Tools.Resident]'s default set: the coding core
// used on nearly every turn, which stays preloaded even under index mode —
// indexing tools this frequently used would buy a search round trip per
// session for nothing. A var, not a const, because Go has no const slices;
// [Tools.ResidentTools] returns a COPY of it so a caller can never mutate
// this package default through the returned slice.
var DefaultResidentTools = []string{
	"bash", "read", "edit", "write", "grep", "glob", "ls",
	"update_plan", "ask_user", "tool_search",
}

const (
	// DefaultToolSummaryBytes is [Tools.SummaryBytes]'s default: 160 bytes —
	// roughly one legible line per indexed tool (~40 tokens), enough for a
	// name and a one-sentence description, not a full schema.
	DefaultToolSummaryBytes = 160
	// DefaultToolSearchResults is [Tools.SearchResults]'s default: 10 — a
	// result set that fits a screen and a reasonable context budget for one
	// tool_search call.
	DefaultToolSearchResults = 10
	// DefaultToolInlineIndexMax is [Tools.InlineIndexMax]'s default: 25.
	// Below this many indexed tools, inlining the whole index is CHEAPER
	// than a search round trip, so index mode has nothing to buy until the
	// candidate tool count exceeds it.
	DefaultToolInlineIndexMax = 25
)

// Tools configures how gofer presents its tool surface — builtins and
// MCP-federated tools alike — to the model: every schema preloaded, or a
// searchable index with schemas resolved on demand. The zero value is fully
// valid: [Tools.Schemas] resolves to preload, so an operator who upgrades
// gets byte-identical requests to before this section existed until they opt
// into index mode.
//
// The numeric defaults below live here, not in a toolindex package — none
// exists yet. A future SDK package mirroring tool-search behavior MUST keep
// its own constants equal to these; a drift check belongs alongside that
// package once it exists (see the PR report for why it is not here yet).
type Tools struct {
	// SchemaMode selects preload vs index; see [ToolSchemaMode]. Read
	// through [Tools.Schemas], which fails safe to preload.
	SchemaMode string `json:"schema_mode,omitempty"`

	// Resident is the set of tool names that stay preloaded in context even
	// under index mode. Empty resolves to [DefaultResidentTools] via
	// [Tools.ResidentTools].
	Resident []string `json:"resident,omitempty"`

	// SummaryBytes caps how many bytes of description ONE indexed tool's
	// entry carries: nil (unset) is [DefaultToolSummaryBytes], an explicit
	// 0 is "no limit", any other value is a byte cap. See
	// [Tools.SummaryLimitBytes].
	SummaryBytes *int `json:"summary_bytes,omitempty"`

	// SearchResults caps how many entries one tool_search call returns: nil
	// (unset) or non-positive resolves to [DefaultToolSearchResults]. See
	// [Tools.SearchResultLimit].
	SearchResults *int `json:"search_results,omitempty"`

	// InlineIndexMax is the tool-count THRESHOLD below which index mode
	// inlines the whole index instead of requiring a search call: nil
	// (unset) or non-positive resolves to [DefaultToolInlineIndexMax]. See
	// [Tools.InlineIndexLimit].
	InlineIndexMax *int `json:"inline_index_max,omitempty"`
}

// Schemas resolves [Tools.SchemaMode] to a [ToolSchemaMode]. Only the exact
// spelling "index" opts into index mode; unset and anything else —
// including a typo or a mode written by a newer gofer — resolve to
// [ToolSchemaModePreload].
//
// This is the OPPOSITE fail-safe polarity from a guardrail knob like
// [Session.Mode]: there, failing safe means failing toward the behavior
// whose worst case is asking a human, never toward running unattended.
// Here, preload's worst case is wasted context bytes — every schema the
// model might need is already in front of it, so preload mode can never
// fail to find a tool. Index mode's worst case is a real capability gap: a
// tool the model cannot find, or resolves one call late. So the fail-safe
// direction here is toward the mode whose failure is COST, never toward the
// mode whose failure is INCAPACITY. It also means this round ships
// byte-identical requests to every existing deployment until an operator
// deliberately opts in.
func (t Tools) Schemas() ToolSchemaMode {
	if ToolSchemaMode(t.SchemaMode) == ToolSchemaModeIndex {
		return ToolSchemaModeIndex
	}
	return ToolSchemaModePreload
}

// ResidentTools resolves [Tools.Resident]'s effective value: a COPY of
// [DefaultResidentTools] when empty, else the configured set. Copying
// prevents a caller from mutating the package default through the returned
// slice — Go has no way to hand back a read-only one.
func (t Tools) ResidentTools() []string {
	if len(t.Resident) > 0 {
		return t.Resident
	}
	out := make([]string, len(DefaultResidentTools))
	copy(out, DefaultResidentTools)
	return out
}

// SummaryLimitBytes resolves [Tools.SummaryBytes]'s effective value:
// [DefaultToolSummaryBytes] when unset or negative, else the explicit
// stored value (0 = no limit).
func (t Tools) SummaryLimitBytes() int {
	if t.SummaryBytes == nil || *t.SummaryBytes < 0 {
		return DefaultToolSummaryBytes
	}
	return *t.SummaryBytes
}

// SearchResultLimit resolves [Tools.SearchResults]'s effective value:
// [DefaultToolSearchResults] when unset or non-positive.
func (t Tools) SearchResultLimit() int {
	if t.SearchResults == nil || *t.SearchResults <= 0 {
		return DefaultToolSearchResults
	}
	return *t.SearchResults
}

// InlineIndexLimit resolves [Tools.InlineIndexMax]'s effective value:
// [DefaultToolInlineIndexMax] when unset or non-positive.
func (t Tools) InlineIndexLimit() int {
	if t.InlineIndexMax == nil || *t.InlineIndexMax <= 0 {
		return DefaultToolInlineIndexMax
	}
	return *t.InlineIndexMax
}
