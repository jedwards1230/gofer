package tui

// mcp_view.go implements the /mcp command-panel tab (gofer#303): a read-only,
// no-persist view over the BACKEND's [capability.Answer], answering "which MCP
// servers do I have, and which of them are actually up right now".
//
// It reads exactly one input — the fetched answer (capabilities.go) — and
// deliberately not [CommandEnv.Config]. That is a correctness property, not
// tidiness: this process's config.json describes THIS machine, and a
// daemon-attached panel that fell back to it would render a fully plausible
// server list belonging to a completely different process. With no config
// reachable from here, the fallback is not merely unwired, it is
// unexpressible. TestDaemonUnknownCapabilitiesNeverRendersLocalSnapshot pins
// it.
//
// It follows status.go's omission rule (see its package doc): a field the
// current data cannot answer honestly is ABSENT, not blank-filled. Three MCP
// fields fall under that rule today, and rather than leave their absence to be
// noticed, the view says so on screen — see [mcpView.omissionLine].

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// mcpView renders the MCP tab. Like every other read-only tab it is a pure
// value constructed inline per render (panel.go's body switch) and holds no
// state of its own.
type mcpView struct {
	theme theme.Theme
	caps  capabilitiesState
}

// View renders the view's rows, width-truncated and capped to height — the
// same Renderable contract every other panel component follows.
func (v mcpView) View(width, height int) string {
	lines := v.lines(height)
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the body rows for whichever of the three capability states the
// panel is in (see [capabilitiesState]).
func (v mcpView) lines(height int) []string {
	switch {
	case v.caps.pending && !v.caps.loaded:
		return loadingCapabilityLines("MCP servers")
	case !v.caps.loaded || !v.caps.answer.Known:
		return unknownCapabilityLines("MCP")
	}

	mcp := v.caps.answer.Snapshot.MCP
	if len(mcp.Servers) == 0 {
		// "none configured" and UNKNOWN above must never read alike — they are
		// opposite claims, and an Ascii golden sees only the words.
		return []string{
			"MCP servers: none configured.",
			"Add one under mcp.servers in config.json.",
		}
	}

	head := []string{v.headerLine(mcp.Servers)}
	rows := make([]string, 0, len(mcp.Servers))
	for _, s := range mcp.Servers {
		rows = append(rows, v.serverLine(s))
	}
	tail := []string{
		"Federated tools: " + strconv.Itoa(mcp.ConnectedTools) + " total across connected servers",
	}
	if line := schemaModeLine(mcp); line != "" {
		tail = append(tail, line)
	}
	if mcp.ConfigDrifted {
		// Above the omission line, because unlike that one this is ACTIONABLE
		// and explains why a server the reader just configured is missing from
		// the list above.
		tail = append(tail, v.theme.WarnStyle().Render("config.json changed since startup — restart gofer to apply"))
	}
	tail = append(tail, v.omissionLine())
	return fitRows(head, rows, tail, height)
}

// headerLine counts the configured servers and how many are up.
func (v mcpView) headerLine(servers []capability.Server) string {
	connected := 0
	for _, s := range servers {
		if s.Connected {
			connected++
		}
	}
	return "MCP servers: " + strconv.Itoa(connected) + " connected of " + strconv.Itoa(len(servers)) + " configured"
}

// serverLine renders one server: name, its CONFIGURED transport, and its
// state.
//
// The state word is chosen so each distinct situation reads differently in
// plain text, since the goldens that pin this view are colour-blind by
// construction:
//
//   - "disabled" — mcp.servers[].enabled is false; never dialed.
//   - "unsupported transport" — the configured transport is one gofer does not
//     recognize, so the manager skips the server entirely. Without this word it
//     would be indistinguishable from a server that is merely down.
//   - "connected" / "not connected" — the manager's snapshot.
//
// "not connected" is deliberately not narrowed further: nothing available
// distinguishes a server that has never once connected from one that connected
// and dropped, and nothing records why either happened (see omissionLine).
func (v mcpView) serverLine(s capability.Server) string {
	transport := s.ConfiguredTransport
	state := ""
	switch {
	case !s.Enabled:
		state = "disabled"
	case transport == "":
		state = "unsupported transport"
	case s.Connected:
		state = "connected"
	default:
		state = "not connected"
	}
	if transport == "" {
		transport = "?"
	}
	line := "  " + padCell(s.Name, 16) + padCell(transport+" (configured)", 20) + state
	switch {
	case s.Connected:
		return v.theme.OKStyle().Render(line)
	case !s.Enabled:
		return v.theme.MutedStyle().Render(line)
	default:
		return v.theme.WarnStyle().Render(line)
	}
}

// omissionLine names what this panel deliberately does NOT show, so its
// absence is a stated limitation rather than something a reader has to notice.
// Naming the issue makes the line self-retiring: when gofer#302 lands, the
// data exists and this line goes with it.
func (v mcpView) omissionLine() string {
	return v.theme.MutedStyle().Render("Not reported (gofer#302): per-server tools, down-reason, down-since")
}

// schemaModeLine renders how the federated tools reach the model — CONFIGURED
// intent computed from tools.schema_mode plus tools.resident, not a reading of
// any live tool index.
//
// Four cases, one per branch:
//
//   - "" — the backend reported no schema mode at all. This is the ONLY case
//     that returns "" and omits the row entirely (status.go's contingent-row
//     idiom: the producer returns "" and the caller drops it). An empty mode
//     means the report predates the field or carried no MCP section; rendering
//     a mode name for it would be inventing one.
//   - "index" — the resident/index-only split, which is the whole reason this
//     row exists.
//   - "preload" — its own line. The row is NOT suppressed here: "every schema
//     is already in context" is a real answer to "how do my MCP tools reach the
//     model", and silence would read as "unknown" rather than "preload".
//   - anything else — named, WITHOUT claiming preload semantics. gofer's own
//     [config.Tools.Schemas] is a closed two-value enum that fails safe to
//     preload, so this is unreachable from a same-version backend — but the
//     mode arrives over the wire from a daemon that may be NEWER than this
//     client (the version skew gofer/hello exists to detect), and asserting
//     "every schema preloaded" about a mode this binary has never heard of
//     would be exactly the plausible-looking wrong answer this panel exists to
//     avoid.
func schemaModeLine(mcp capability.MCP) string {
	switch mcp.SchemaMode {
	case "":
		return ""
	case "index":
		return "Tool schemas: index (configured) — " +
			strconv.Itoa(mcp.IndexOnlyTools) + " index-only, " +
			strconv.Itoa(mcp.ResidentTools) + " resident"
	case "preload":
		return "Tool schemas: preload (configured) — every schema preloaded"
	default:
		return "Tool schemas: " + mcp.SchemaMode + " (configured) — unrecognized by this gofer"
	}
}

// padCell right-pads s to at least n columns, leaving an over-long value intact
// (the row is truncated to the panel width anyway) so a long server name
// pushes its neighbours right instead of being silently cut mid-name.
func padCell(s string, n int) string {
	w := ansi.StringWidth(s)
	if w >= n {
		return s + " "
	}
	return s + strings.Repeat(" ", n-w)
}
