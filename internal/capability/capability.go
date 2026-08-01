// Package capability carries the read-only runtime capability report the
// TUI's /mcp and /skills command-panel tabs render (gofer#303): which MCP
// servers are configured and which of them currently hold a live connection,
// how the tool surface is presented to the model, and which SKILL.md skills a
// session created here would load.
//
// It is a stdlib-only leaf so the three layers that touch the report — the
// producer (internal/supervisor), the wire (internal/daemon), and the renderer
// (internal/tui) — share ONE set of types instead of three that drift.
//
// # Every field here is one the current data can answer
//
// The whole point of this package is what it does NOT carry. It is built on
// [github.com/jedwards1230/gofer/internal/mcpconn.Manager.Snapshot], whose
// result is a FLAT union of every connected server's tools plus the names of
// the servers that are down. From that, three questions have no truthful
// answer today, and each is therefore ABSENT from these types rather than
// present-and-guessed:
//
//   - **Per-server tool attribution.** Snapshot's tool list is flat and the
//     proxy tool's owning server is unexported. Reconstructing it by parsing
//     the `mcp__<server>__<tool>` name prefix would produce a plausible answer
//     that is wrong the moment a server's projection changes, so there is no
//     per-server tool list or per-server tool count. [MCP.ConnectedTools] is a
//     TOTAL across connected servers and is named to say so.
//   - **Never-connected vs connected-then-dropped.** Snapshot's Down carries
//     both, undifferentiated (its own doc says so), so [Server.Connected] is a
//     plain bool with no history behind it.
//   - **Why a server is down.** Connect and tools/list failures are logged and
//     dropped by the Manager; nothing stores them. There is no failure-reason
//     field.
//
// Fixing that is gofer#302's job, in [github.com/jedwards1230/gofer/internal/mcpconn].
// When it lands, these types gain fields — until then, a renderer must say the
// answers are unavailable rather than invent them.
package capability

// Snapshot is one backend's answer about its own runtime capabilities. The
// zero value is a valid "nothing configured" answer — which is NOT the same
// as "this backend cannot tell you", a distinction [Answer] carries.
type Snapshot struct {
	MCP    MCP    `json:"mcp"`
	Skills Skills `json:"skills"`
}

// MCP is the MCP half of a [Snapshot]: the configured servers, the total
// federated tool count, and how those tools are presented to the model.
type MCP struct {
	// Servers is every CONFIGURED server, in config order — including
	// disabled ones and ones whose transport gofer does not recognize, since
	// "configured but not running" is exactly what an operator opens this
	// panel to see.
	Servers []Server `json:"servers,omitempty"`

	// ConnectedTools is the TOTAL number of federated tools across every
	// currently-connected server — never a per-server figure, because the
	// snapshot this is derived from cannot attribute a tool to a server (see
	// the package doc).
	ConnectedTools int `json:"connected_tools"`

	// SchemaMode is the resolved tools.schema_mode ("preload" or "index").
	SchemaMode string `json:"schema_mode,omitempty"`

	// ResidentTools and IndexOnlyTools split ConnectedTools by whether a tool
	// is in the configured resident set, and are populated ONLY under index
	// mode (under preload every tool's schema is in context and the split is
	// meaningless). They are CONFIGURED INTENT computed from config plus the
	// tool names, not a reading of any live tool index — the live index is not
	// reachable from outside a session.
	ResidentTools  int `json:"resident_tools"`
	IndexOnlyTools int `json:"index_only_tools"`
}

// Server is one configured MCP server's state.
type Server struct {
	// Name is the configured server name.
	Name string `json:"name"`

	// ConfiguredTransport is [github.com/jedwards1230/gofer/internal/config.MCPServer.Transport]'s
	// resolved value ("stdio", "http", or "" for a transport gofer does not
	// recognize). It is what the OPERATOR configured, not something observed
	// on a live connection — a renderer must label it that way.
	ConfiguredTransport string `json:"configured_transport,omitempty"`

	// Enabled is the configured mcp.servers[].enabled (default true).
	Enabled bool `json:"enabled"`

	// Connected reports whether this server held a live connection at snapshot
	// time. It is false for a disabled server, for one whose transport is
	// unrecognized (the manager never attempts either), and for an enabled
	// server that is down — and NOTHING here distinguishes a server that has
	// never once connected from one that connected and dropped, because the
	// underlying snapshot does not either (see the package doc).
	Connected bool `json:"connected"`
}

// Skills is the skills half of a [Snapshot].
type Skills struct {
	// Directories is the resolved discovery order — first directory to define
	// a name wins (PATH-style; see internal/config.Skills.Directories).
	Directories []string `json:"directories,omitempty"`

	// Loaded is every discovered skill, sorted by name.
	Loaded []Skill `json:"loaded,omitempty"`

	// Diagnostics is every candidate the loader could not turn into a skill,
	// or dropped as a duplicate — verbatim, never filtered.
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`

	// Summary is skillset.Summarize's one-line operator note over Diagnostics,
	// "" when there were none.
	Summary string `json:"summary,omitempty"`
}

// Skill is one loaded skill's index entry.
//
// It deliberately carries NO source path: the SDK's skill.Meta records none,
// and the only way to produce one would be to re-stat the discovery
// directories and guess which candidate won — a reconstruction that goes wrong
// exactly when a first-directory candidate failed to load for an unrelated
// reason. The LOSING side of a shadowing IS knowable, and is reported through
// [Diagnostic.Shadowed].
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Truncated reports that Description was cut to the configured budget.
	Truncated bool `json:"truncated,omitempty"`
	// Disabled reports that skills.disabled names this skill: it is loaded but
	// excluded from the model-facing projection.
	Disabled bool `json:"disabled,omitempty"`
}

// Diagnostic is one skill-loader diagnostic, flattened for the wire.
type Diagnostic struct {
	// Path is the file or directory the diagnostic is about.
	Path string `json:"path"`
	// Detail is the loader's reason, verbatim.
	Detail string `json:"detail"`
	// Shadowed reports that this candidate lost a name to an earlier
	// directory — see skillset.IsShadowed for how that is recognized and how
	// it degrades if the SDK rewords the message.
	Shadowed bool `json:"shadowed,omitempty"`
}

// Answer is a [Snapshot] plus whether the backend could produce one at all.
//
// The two-level shape exists because the zero Snapshot is genuinely ambiguous:
// it is both "no MCP servers and no skills are configured" and "I have no idea".
// Known separates them, and a renderer MUST render the two differently — an
// unknown answer drawn as an empty list reads as "nothing configured", which is
// precisely the lie this type exists to prevent.
//
// Known is false for a daemon that predates gofer/capabilities, and for a
// `gofer daemon --workers` router — which owns no supervisor and therefore no
// MCP manager at all (each session's worker process owns its own).
type Answer struct {
	Known    bool     `json:"known"`
	Snapshot Snapshot `json:"snapshot"`
}
