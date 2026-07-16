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
	"time"

	"github.com/jedwards1230/gofer/internal/config"
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

	// Auth lists every authenticated provider, wrapping auth.Store.Status().
	Auth func() ([]ProviderAuth, error)

	// Config loads the store root's config.json, wrapping config.Load.
	Config func() (config.Config, error)

	// SaveConfig persists a full config.Config to the store root's
	// config.json, wrapping config.Save. /config (config_view.go) calls this
	// on every committed edit — bool toggle, enum cycle, or a string edit's
	// Enter — immediately, with no separate save step.
	SaveConfig func(config.Config) error
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
