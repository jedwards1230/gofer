// Package config is gofer's native on-disk configuration. M3 defines only the
// permissions block — the ruleset gofer's guard consults before it runs a tool
// call. A vendor-format import (Claude Code settings.json) is deliberately NOT
// here: that lands in M4/M5 (see the SDK's permission package doc). More config
// sections (models, plugins, …) join this type in later milestones.
//
// The file format is JSON, read from <root>/config.json (see [DefaultPath]).
// A missing file is not an error — an unconfigured gofer runs the default
// contain-or-ask policy (see [Config.Engine]).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/permission"

	"github.com/jedwards1230/gofer/internal/telemetry"
)

// ConfigFileName is the base name of gofer's config file under the store root.
const ConfigFileName = "config.json"

// Config is gofer's parsed configuration file.
type Config struct {
	// Permissions is the native permission ruleset, evaluated with the SDK's
	// deny>ask>allow precedence on top of gofer's default contain-or-ask
	// catch-all (see [Config.Engine]).
	Permissions []Rule `json:"permissions,omitempty"`
	// Telemetry is gofer's OpenTelemetry configuration block. The zero value
	// is disabled — see [Telemetry.ToTelemetry].
	Telemetry Telemetry `json:"telemetry,omitempty"`
}

// Telemetry is gofer's native OpenTelemetry configuration block, mirroring
// [telemetry.Config]'s fields for JSON persistence. The zero value is fully
// valid and disabled (see [Telemetry.ToTelemetry]) — an unconfigured gofer
// exports no traces or metrics.
type Telemetry struct {
	Enabled     bool              `json:"enabled,omitempty"`
	Endpoint    string            `json:"endpoint,omitempty"`
	Protocol    string            `json:"protocol,omitempty"`
	ServiceName string            `json:"service_name,omitempty"`
	Insecure    bool              `json:"insecure,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// ToTelemetry converts the config block into a [telemetry.Config]. Follows
// the same "zero value yields a sane (here: disabled) default" pattern as
// [Config.Engine] — a config file with no telemetry block, or no config file
// at all, compiles to telemetry.Config{}, which [telemetry.Setup] treats as
// off.
func (t Telemetry) ToTelemetry() telemetry.Config {
	return telemetry.Config{
		Enabled:     t.Enabled,
		Endpoint:    t.Endpoint,
		Protocol:    t.Protocol,
		ServiceName: t.ServiceName,
		Insecure:    t.Insecure,
		Headers:     t.Headers,
	}
}

// Rule is one native permission rule: a Verdict (allow|ask|deny) applied to a
// Tool + Specifier. Tool ""/"*" matches any tool; Specifier ""/"*" matches any
// target, a "prefix:*" specifier matches by command/target prefix, otherwise it
// is a path.Match glob — the SDK's [permission.Rule] grammar this compiles to.
type Rule struct {
	Verdict   string `json:"verdict"`
	Tool      string `json:"tool,omitempty"`
	Specifier string `json:"specifier,omitempty"`
}

// DefaultPath returns the config file path for a store root: <root>/config.json.
func DefaultPath(root string) string { return filepath.Join(root, ConfigFileName) }

// Load reads and parses the gofer config file at path. A missing file is NOT an
// error: it returns the zero Config, whose [Config.Engine] is the default
// contain-or-ask policy, so an unconfigured gofer still runs. A present but
// malformed or invalid file IS an error — a typo in a permission rule must fail
// loudly rather than silently widening or narrowing the policy.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// validate rejects a rule with an unrecognized verdict, so a typo ("den")
// surfaces at load rather than silently never matching.
func (c Config) validate() error {
	for i, r := range c.Permissions {
		switch event.Verdict(r.Verdict) {
		case event.VerdictAllow, event.VerdictAsk, event.VerdictDeny:
		default:
			return fmt.Errorf("permissions[%d]: unknown verdict %q (want allow, ask, or deny)", i, r.Verdict)
		}
	}
	return nil
}

// Engine compiles the config into an SDK [permission.Engine] carrying gofer's
// default policy: contain-or-ask.
//
// A catch-all allow rule is seeded FIRST, so a call no config rule matches
// resolves to allow — which the guard's [loop.RuleGuard] then routes through the
// sandbox Container (run-contained when containable, else ask a human). The
// config's own rules are appended after; because the engine evaluates deny
// before ask before allow, a config deny or ask rule for a given tool+specifier
// wins over the default catch-all allow, while unmatched calls keep the
// contain-or-ask default. An empty config therefore yields "allow everything →
// contain-or-ask", never "run everything uncontained".
func (c Config) Engine() *permission.Engine {
	rules := make([]permission.Rule, 0, len(c.Permissions)+1)
	rules = append(rules, permission.Rule{
		Verdict:   event.VerdictAllow,
		Tool:      "*",
		Specifier: "*",
		Source:    "default",
	})
	for _, r := range c.Permissions {
		rules = append(rules, permission.Rule{
			Verdict:   event.Verdict(r.Verdict),
			Tool:      r.Tool,
			Specifier: r.Specifier,
			Source:    "config",
		})
	}
	return permission.New(rules...)
}
