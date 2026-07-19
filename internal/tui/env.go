package tui

// env.go is the command panel's read-only data seam (M4 step 2): the
// version/cwd/store-root identity plus lazy auth/config reads a view needs
// but [App] doesn't otherwise hold. cmd/gofer builds one [CommandEnv] per
// process from the resolved store root — the same way [OverviewMeta]'s
// fields are threaded in — and passes it to [NewApp]; [App] hands it to the
// command panel at open time (see command.go's openPanel), and a view reads
// it lazily on every render rather than caching a snapshot, so state that
// changed elsewhere (a `/login` in another terminal, an edited config.json)
// shows up the next time the panel opens with no extra plumbing.

import (
	"context"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/modelcatalog"
)

// CommandEnv is the command panel's read-only data source. Auth and Config
// are closures, not cached values, so every read reflects the current
// on-disk state. Both are non-fatal to call: a missing/malformed file is a
// normal state a view renders around ("not signed in", defaults) rather than
// an error that blocks the panel from opening — every view must open cleanly
// with zero providers authenticated. The zero CommandEnv (nil
// Auth/Config/SaveConfig) is valid and answers "nothing configured" for
// Auth/Config; SaveConfig nil is a no-op an edit-committing view must guard
// before calling.
type CommandEnv struct {
	Version string
	Cwd     string
	Root    string // the resolved store root (~/.gofer, or --root)

	// DaemonBacked reports whether the roster this TUI drives belongs to a
	// running `gofer daemon` rather than to a local in-process supervisor this
	// process owns (cmd/gofer's selectTUIBackend picks one or the other).
	//
	// It exists because it changes what a config write MEANS. Auth and Config
	// are always local to this machine, but the daemon resolves its own default
	// model exactly once, at ITS startup, and never re-reads config.json — so
	// from a daemon-attached TUI, persisting session.model cannot change what
	// the attached daemon runs, only what a future one will (issue #156). The
	// only consumer is [App.handleModelSelect], which must not report an effect
	// that did not occur. False (the zero value) is the local backend, where a
	// config write does take effect immediately.
	DaemonBacked bool

	// Auth lists every authenticated provider, wrapping auth.Store.Status().
	Auth func() ([]ProviderAuth, error)

	// Config loads the store root's config.json, wrapping config.Load.
	Config func() (config.Config, error)

	// SaveConfig persists a full config.Config to the store root's
	// config.json, wrapping config.Save. /config (config_view.go) calls this
	// on every committed edit — bool toggle, enum cycle, or a string edit's
	// Enter — immediately, with no separate save step.
	SaveConfig func(config.Config) error

	// Models resolves the models providerID's CURRENT credential can actually
	// reach, wrapping modelcatalog.Catalog with live discovery enabled.
	//
	// It is the ONE closure here that can perform network IO, and it is the
	// reason /model resolves its list once at panel-open rather than per
	// render: [modelcatalog.DefaultDiscoveryTimeout] bounds it at 3s, which is
	// fine for a one-shot background load and unusable on a keystroke path.
	// It takes a ctx for that reason — the caller runs it off the Update loop
	// in a tea.Cmd and can cancel it — and gofer's store root stays cmd/gofer's
	// business, exactly as it is for Auth/Config.
	//
	// Non-fatal like the rest: [modelcatalog.Catalog] already degrades every
	// discovery failure to the compiled-in floor internally, so an error here
	// means only a broken auth.json. A nil closure (the zero CommandEnv) is
	// valid and leaves the picker on the floor it seeded itself with — never
	// an empty list.
	Models func(ctx context.Context, providerID string) ([]modelcatalog.Model, error)
}

// AuthKind is a credential's kind, mirroring the SDK auth package's CredKind
// ("oauth" | "api_key") without internal/tui importing agent-sdk-go/auth
// directly — the same local-mirror pattern [SessionInfo] uses for the
// supervisor's session shape (see supervisor.go's package doc).
type AuthKind string

const (
	// KindOAuth is a subscription OAuth access token (mirrors auth.KindOAuth).
	KindOAuth AuthKind = "oauth"
	// KindAPIKey is a long-lived API key (mirrors auth.KindAPIKey).
	KindAPIKey AuthKind = "api_key"
)

// ProviderAuth is one authenticated provider — [CommandEnv.Auth]'s element
// type, wrapping the SDK's auth.StatusEntry. It carries no org/email/account:
// the SDK's StatusEntry doesn't either (an opaque OpenAI chatgpt-account-id
// aside), so gofer is multi-provider with no single "logged in as" identity
// to show.
type ProviderAuth struct {
	Provider string
	Kind     AuthKind
	Expires  time.Time // zero for an API key, or an OAuth entry with no expiry
	Expired  bool      // OAuth only
}
