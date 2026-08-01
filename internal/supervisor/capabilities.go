package supervisor

// capabilities.go produces the read-only runtime capability report the TUI's
// /mcp and /skills panels render (gofer#303). It lives in this package — not
// beside the views — because the two things it reads are unexported state of
// *[Supervisor]: the process-lifetime [mcpconn.Manager] built in New, and the
// store root the skill loader resolves its global directory against.
//
// It ADDS no data. Every MCP field here is a projection of what
// [mcpconn.Manager.Snapshot] already returns; the three questions that
// snapshot cannot answer (per-server tool attribution, never-connected vs
// dropped, and why a server is down) are absent from [capability.Snapshot]
// rather than guessed. Enriching the snapshot itself is gofer#302's job.

import (
	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/skillset"
)

// Capabilities reports this supervisor's current MCP and skills state, as a
// session created in cwd would see it.
//
// cwd is a parameter rather than supervisor state because it genuinely varies
// per session: the project skills directory is `<cwd>/.gofer/skills`, and this
// supervisor hosts sessions in many directories at once. A caller asking "what
// would a session started HERE load" passes its own working directory; an
// empty cwd simply resolves no project directory, which is a normal answer and
// not an error.
//
// It is a point-in-time read with no promise of staying current, exactly like
// the snapshot beneath it: a server may connect or drop a millisecond later,
// and a SKILL.md may be edited between two calls. Every failure mode collapses
// to the empty answer — the skill loader reports a bad candidate as a
// diagnostic rather than an error, and a Manager with nothing configured
// snapshots empty — so this never fails and never blocks a panel from opening.
func (s *Supervisor) Capabilities(cwd string) capability.Snapshot {
	return capability.Snapshot{
		MCP:    s.mcpCapabilities(),
		Skills: s.skillCapabilities(cwd),
	}
}

// mcpCapabilities projects the MCP manager's snapshot plus the configured
// server list into [capability.MCP].
//
// Connectedness is derived by MEMBERSHIP, not by inference: a server counts as
// connected only when the manager is actually managing it
// ([config.MCP.EnabledServers] — which excludes both disabled servers and ones
// whose transport gofer does not recognize) AND it is absent from the
// snapshot's Down list. Computing it as "enabled and not in Down" instead
// would report a server the manager never even attempted — an unrecognized
// transport, say — as connected, since a server that was never dialed cannot
// appear in Down either.
func (s *Supervisor) mcpCapabilities() capability.MCP {
	cfg := s.mcpConfig()

	var out capability.MCP
	down := map[string]bool{}
	managed := map[string]bool{}
	if s.mcpManager != nil {
		snap := s.mcpManager.Snapshot()
		out.ConnectedTools = len(snap.Tools)
		for _, name := range snap.Down {
			down[name] = true
		}
		for _, srv := range cfg.EnabledServers() {
			managed[srv.Name] = true
		}
		out.ResidentTools, out.IndexOnlyTools, out.SchemaMode = s.toolSplit(snap.Tools)
	}

	out.Servers = make([]capability.Server, 0, len(cfg.Servers))
	for _, srv := range cfg.Servers {
		out.Servers = append(out.Servers, capability.Server{
			Name:                srv.Name,
			ConfiguredTransport: string(srv.Transport()),
			Enabled:             srv.IsEnabled(),
			Connected:           managed[srv.Name] && !down[srv.Name],
		})
	}
	return out
}

// toolSplit resolves tools.schema_mode and, under index mode only, splits
// tools by whether the configured resident set names them.
//
// This is CONFIGURED INTENT, deliberately: it recomputes the same rule
// sessionGuard applies when it builds a session's toolindex, from the same
// config, rather than reading any live index — the live *toolindex.Index
// belongs to one session's registry and is unreachable from here. Under
// preload mode both counts stay zero: every schema is in context, so the split
// describes nothing.
func (s *Supervisor) toolSplit(tools []tool.Tool) (resident, indexOnly int, mode string) {
	cfg := s.toolsConfig()
	mode = string(cfg.Schemas())
	if cfg.Schemas() != config.ToolSchemaModeIndex {
		return 0, 0, mode
	}
	residentSet := map[string]bool{}
	for _, name := range cfg.ResidentTools() {
		residentSet[name] = true
	}
	for _, t := range tools {
		if residentSet[t.Name()] {
			resident++
			continue
		}
		indexOnly++
	}
	return resident, indexOnly, mode
}

// skillCapabilities re-runs the skill discovery a session create would run and
// projects the result. It is a pure filesystem walk with no session state
// behind it, which is what makes it safe to run per panel-open — and it is
// [skillset.Summarize]'s first caller anywhere: sessionGuard has always
// computed that line and then dropped it into a note nothing rendered.
func (s *Supervisor) skillCapabilities(cwd string) capability.Skills {
	cfg := s.skillsConfig()
	set, diags := skillset.Load(cfg, s.root, cwd)

	out := capability.Skills{
		Directories: cfg.Directories(s.root, cwd),
		Summary:     skillset.Summarize(diags),
	}
	index := set.Index()
	out.Loaded = make([]capability.Skill, 0, len(index))
	for _, m := range index {
		out.Loaded = append(out.Loaded, capability.Skill{
			Name:        m.Name,
			Description: m.Description,
			Truncated:   m.Truncated,
			Disabled:    cfg.IsDisabled(m.Name),
		})
	}
	out.Diagnostics = make([]capability.Diagnostic, 0, len(diags))
	for _, d := range diags {
		detail := ""
		if d.Err != nil {
			detail = d.Err.Error()
		}
		out.Diagnostics = append(out.Diagnostics, capability.Diagnostic{
			Path:     d.Path,
			Detail:   detail,
			Shadowed: skillset.IsShadowed(d),
		})
	}
	return out
}
