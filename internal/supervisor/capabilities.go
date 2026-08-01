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
//
// The config SECTIONS are resolved independently (mcpConfig / toolsConfig /
// skillsConfig are separate closures, each typically its own config.Load), so a
// config.Save landing mid-report can produce a view that mixes an old section
// with a new one. That is inherent to the per-section resolver design and is
// what session creation already does (see sessionGuard, which resolves the same
// four sections one at a time); consolidating it belongs with that shape, not
// here. The blast radius is one render of one read-only panel, and the next
// open is consistent again.
func (s *Supervisor) Capabilities(cwd string) capability.Snapshot {
	return capability.Snapshot{
		MCP:    s.mcpCapabilities(),
		Skills: s.skillCapabilities(cwd),
	}
}

// mcpCapabilities projects the MCP manager's snapshot into [capability.MCP].
//
// # It describes the MANAGER, never the current file
//
// Every server field here comes from [Supervisor.mcpAtStart] — the config the
// Manager was actually built from — and NOT from s.mcpConfig(), which in
// production (cmd/gofer's mcpConfigResolver) is a fresh config.Load on every
// call.
//
// That distinction was a live fabrication before it was a rule. The Manager's
// server set is fixed at New and Snapshot's Down only ever iterates it, so a
// server ADDED to config.json after startup was absent from Down for the
// trivial reason that the Manager had never heard of it — and a "connected"
// derived from the live config read that absence as health and rendered a green
// "connected" for a server that had never been dialed. It failed in exactly the
// situation that sends someone to this panel: add an MCP server, open gofer,
// type /mcp. The mirror case was equally wrong — a server deleted from the file
// vanished from the list while its tools stayed in the federated count.
//
// Connectedness is then derived by MEMBERSHIP, never by inference: a server
// counts as connected only when the Manager is actually managing it
// (EnabledServers, which excludes both disabled servers and ones whose
// transport gofer does not recognize) AND it is absent from Down. Computing it
// as "enabled and not in Down" would report a server the Manager never
// attempted as connected, since one that was never dialed cannot appear in Down
// either.
//
// The live file is read for exactly one purpose: to report that it has DRIFTED
// (see [capability.MCP.ConfigDrifted]). That is a comparison of two values in
// hand, not a guess about the Manager.
func (s *Supervisor) mcpCapabilities() capability.MCP {
	cfg := s.mcpAtStart

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
	out.ConfigDrifted = mcpServersDiffer(cfg, s.mcpConfig())
	return out
}

// mcpServersDiffer reports whether the live mcp.servers list differs from the
// one the Manager was built from, in any way that would change what it
// connects: the set of names, their order, their enabled state, or their
// resolved transport.
//
// This is the honest, actionable half of the drift problem. Suppressing the
// added server from the list is correct — the Manager genuinely has no state
// for it — but silently omitting it would leave an operator who just edited
// config.json staring at a panel that does not mention their server and does
// not say why. Timeout-only fields are ignored deliberately: they change how
// the Manager behaves, not WHICH servers it holds, so flagging them would train
// the reader to ignore the notice.
func mcpServersDiffer(atStart, live config.MCP) bool {
	if len(atStart.Servers) != len(live.Servers) {
		return true
	}
	for i, a := range atStart.Servers {
		b := live.Servers[i]
		if a.Name != b.Name || a.IsEnabled() != b.IsEnabled() || a.Transport() != b.Transport() {
			return true
		}
	}
	return false
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

	dirs := cfg.Directories(s.root, cwd)
	if cwd == "" && len(cfg.Dirs) == 0 {
		// An empty cwd is a normal request (capabilitiesParams.Cwd is
		// omitempty, so any client that omits it lands here), but it must not
		// be answered by GUESSING a project directory. Directories joins cwd
		// unconditionally, so "" yields the RELATIVE ".gofer/skills" — resolved
		// against whatever working directory this process happens to have, and
		// then rendered verbatim in the panel's "Searched, in precedence order"
		// list as though it were the caller's project. Drop the project entry
		// and answer about the store root alone, which is the only half the
		// caller actually asked about.
		//
		// Only for the DEFAULT layout: an explicit skills.dirs is the
		// operator's own list and is never cwd-derived, so it is passed through
		// untouched.
		dirs = dirs[1:]
	}
	set, diags := skillset.LoadDirs(cfg, dirs)

	out := capability.Skills{
		Directories: dirs,
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
