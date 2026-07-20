package tui

// settings.go is the setting registry the /config view (config_view.go)
// renders and edits through — the config-domain parallel to command.go's
// slash-command registry. Every row is a [Setting]: a namespaced key, a
// display Label, a Kind that picks the edit affordance (bool toggles, enum
// cycles, string opens an inline edit line), and a Get/Set pair reading and
// writing a [config.Config] through its own string representation (so the
// view never switches on Kind to know how to read/write a value — see
// config_view.go's use of Get/Set). Namespacing (session.*, tui.*,
// telemetry.*, and eventually plugin.<name>.*) is a registration-time
// concern: a plugin can add rows under plugin.<name>.<key> once plugin
// loading exists (M7) without this type changing shape.

import "github.com/jedwards1230/gofer/internal/config"

// SettingKind selects a [Setting]'s edit affordance.
type SettingKind int

const (
	// SettingBool toggles between "true" and "false" in place.
	SettingBool SettingKind = iota
	// SettingEnum cycles through Options, wrapping.
	SettingEnum
	// SettingString opens an inline edit line.
	SettingString
)

// Setting is one row in the registry: Key is the namespaced identifier
// (e.g. "session.model") the view filters and displays by; Get/Set read and
// write the value through its canonical string form, so a bool renders as
// "true"/"false" and an enum as one of Options — the same representation the
// view edits in place, regardless of Kind.
type Setting struct {
	Key     string
	Label   string
	Kind    SettingKind
	Options []string // enum only; ignored for bool/string

	Get func(config.Config) string
	Set func(config.Config, string) config.Config
}

// settingsRegistry returns the M4 settings table — the concrete initial set
// from docs/projects/gofer-m4-command-views-plan.md §3a. Adding a setting is
// one row here; the view (config_view.go) never special-cases a key by name.
func settingsRegistry() []Setting {
	return []Setting{
		{
			Key:   "session.model",
			Label: "Default model",
			Kind:  SettingString,
			Get:   func(c config.Config) string { return c.Session.Model },
			Set: func(c config.Config, v string) config.Config {
				c.Session.Model = v
				return c
			},
		},
		{
			Key:     "session.permission_mode",
			Label:   "Permission mode",
			Kind:    SettingEnum,
			Options: []string{"ask", "yolo"},
			Get: func(c config.Config) string {
				if c.Session.PermissionMode == "" {
					return "ask"
				}
				return c.Session.PermissionMode
			},
			Set: func(c config.Config, v string) config.Config {
				c.Session.PermissionMode = v
				return c
			},
		},
		{
			Key:     "tui.roster_view",
			Label:   "Roster view",
			Kind:    SettingEnum,
			Options: []string{"flat", "grouped"},
			Get: func(c config.Config) string {
				if c.TUI.RosterView == "" {
					return "flat"
				}
				return c.TUI.RosterView
			},
			Set: func(c config.Config, v string) config.Config {
				c.TUI.RosterView = v
				return c
			},
		},
		{
			Key:   "tui.autoscroll",
			Label: "Auto-scroll transcript",
			Kind:  SettingBool,
			Get: func(c config.Config) string {
				if c.TUI.AutoscrollEnabled() {
					return "true"
				}
				return "false"
			},
			Set: func(c config.Config, v string) config.Config {
				enabled := v == "true"
				c.TUI.Autoscroll = &enabled
				return c
			},
		},
		{
			Key:   "tui.mouse",
			Label: "Mouse capture (scroll + selection)",
			Kind:  SettingBool,
			Get: func(c config.Config) string {
				if c.TUI.MouseEnabled() {
					return "true"
				}
				return "false"
			},
			Set: func(c config.Config, v string) config.Config {
				enabled := v == "true"
				c.TUI.Mouse = &enabled
				return c
			},
		},
		{
			Key:   "telemetry.enabled",
			Label: "Telemetry enabled",
			Kind:  SettingBool,
			Get: func(c config.Config) string {
				if c.Telemetry.Enabled {
					return "true"
				}
				return "false"
			},
			Set: func(c config.Config, v string) config.Config {
				c.Telemetry.Enabled = v == "true"
				return c
			},
		},
		{
			Key:   "telemetry.endpoint",
			Label: "Telemetry endpoint",
			Kind:  SettingString,
			Get:   func(c config.Config) string { return c.Telemetry.Endpoint },
			Set: func(c config.Config, v string) config.Config {
				c.Telemetry.Endpoint = v
				return c
			},
		},
	}
}
