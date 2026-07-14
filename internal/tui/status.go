package tui

// status.go implements the /status command-panel tab (M4 step 2): a
// read-only, no-persist view over [CommandEnv] and whichever session is
// currently peeked or attached, if any. Every field the current data can't
// answer honestly is OMITTED rather than blank-filled — this includes the
// whole MCP row, which gofer has no integration for yet. It never resolves a
// credential or hits a provider — env.Auth/env.Config are local reads only,
// so the view opens cleanly with zero providers authenticated.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// statusView renders the Status tab: version/cwd, session identity, one row
// per authenticated provider (never a singular login/org/email block — gofer
// is multi-provider), the resolved model, and which config layers exist on
// disk. It is a pure value like every other TUI component: env's Auth/Config
// closures are invoked fresh on every render, never cached here, so a /login
// or config edit made elsewhere shows up without any extra plumbing.
type statusView struct {
	theme theme.Theme
	env   CommandEnv
	sess  *SessionInfo // nil on the overview — no active session

	// defaultModel is the roster header's resolved credential-driven default
	// ([Overview.DefaultModel]) — the Model row's fallback when sess is nil or
	// carries no model override.
	defaultModel string
}

// View renders the view's fields, one per line, width-truncated and capped
// to height — the same Renderable contract every other panel/screen
// component follows ([testkit.Renderable]).
func (v statusView) View(width, height int) string {
	lines := v.lines()
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the field rows in table order, omitting any row the current
// data can't answer rather than rendering it blank.
func (v statusView) lines() []string {
	out := []string{
		"Version: " + orDash(v.env.Version),
		"Session: " + v.sessionName(),
		"Session ID: " + v.sessionID(),
		"Cwd: " + orDash(v.env.Cwd),
	}
	out = append(out, v.providerLines()...)
	out = append(out, "Model: "+v.modelLine())
	if src := v.settingSourcesLine(); src != "" {
		out = append(out, src)
	}
	return out
}

// sessionName is the Session-name row: sess.Title when attached/peeked, "—"
// on the overview or if the session carries no title yet.
func (v statusView) sessionName() string {
	if v.sess == nil || v.sess.Title == "" {
		return "—"
	}
	return v.sess.Title
}

// sessionID is the Session-ID row: only populated when attached/peeked.
func (v statusView) sessionID() string {
	if v.sess == nil || v.sess.ID == "" {
		return "—"
	}
	return v.sess.ID
}

// providerLines renders the Providers row(s): a single "not signed in" line
// when env.Auth returns none — including on error, since a read failure is
// non-fatal here too — otherwise a header line plus one indented row per
// authenticated provider, sorted by name for a stable render.
func (v statusView) providerLines() []string {
	auths := v.authList()
	if len(auths) == 0 {
		return []string{v.theme.WarnStyle().Render("Providers: not signed in")}
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].Provider < auths[j].Provider })
	lines := make([]string, 0, len(auths)+1)
	lines = append(lines, "Providers:")
	for _, p := range auths {
		lines = append(lines, "  "+v.providerLine(p))
	}
	return lines
}

// authList reads env.Auth, treating a nil closure (the zero CommandEnv) or a
// read error identically to "no providers" — never a reason to block the
// view.
func (v statusView) authList() []ProviderAuth {
	if v.env.Auth == nil {
		return nil
	}
	auths, err := v.env.Auth()
	if err != nil {
		return nil
	}
	return auths
}

// providerLine renders one provider's name + kind, colored by whether its
// credential is currently usable: OK for an API key or a live OAuth token,
// Danger for an expired one — mirroring `gofer auth`'s STATUS column
// (cmd/gofer/login.go's statusText) at the panel's tighter width budget.
func (v statusView) providerLine(p ProviderAuth) string {
	style := v.theme.OKStyle()
	if p.Kind == KindOAuth && p.Expired {
		style = v.theme.DangerStyle()
	}
	return style.Render(p.Provider + "  " + authKindLabel(p))
}

// authKindLabel renders a provider's kind plus, for OAuth, its expiry.
func authKindLabel(p ProviderAuth) string {
	if p.Kind != KindOAuth {
		return "API key"
	}
	if p.Expired {
		return "OAuth (expired)"
	}
	if p.Expires.IsZero() {
		return "OAuth"
	}
	return "OAuth (valid until " + p.Expires.Format(time.RFC3339) + ")"
}

// modelLine resolves the active model: the attached/peeked session's model
// override, else the roster header's resolved default
// ([Overview.DefaultModel]), else "—".
func (v statusView) modelLine() string {
	if v.sess != nil && v.sess.Model != "" {
		return v.sess.Model
	}
	if v.defaultModel != "" {
		return v.defaultModel
	}
	return "—"
}

// settingSourcesLine renders which config layers actually exist on disk —
// the project `.gofer/config.json` next to env.Cwd and/or `<root>/config.json`
// — omitted entirely (returns "") when neither is present (not a blank row).
// It also reads env.Config, mirroring env.Auth's lazy, non-fatal read: a
// malformed root config surfaces as a note here instead of silently
// vanishing, but a read error never blocks the row from rendering.
func (v statusView) settingSourcesLine() string {
	var layers []string
	if v.env.Cwd != "" && fileExists(filepath.Join(v.env.Cwd, ".gofer", config.ConfigFileName)) {
		layers = append(layers, "project")
	}
	if v.env.Root != "" && fileExists(config.DefaultPath(v.env.Root)) {
		layers = append(layers, "~/.gofer")
	}
	if len(layers) == 0 {
		return ""
	}
	line := "Config: " + strings.Join(layers, ", ")
	if v.env.Config != nil {
		if _, err := v.env.Config(); err != nil {
			line += " (unreadable)"
		}
	}
	return line
}

// fileExists reports whether path names a regular, readable file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// orDash returns s, or "—" if empty.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
