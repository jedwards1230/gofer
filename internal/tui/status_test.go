package tui

// status_test.go covers the /status command-panel tab (status.go, M4 step
// 2): the field-by-field mapping — version/cwd, session identity on the
// overview vs attached, per-provider auth (zero/one/many, including an
// expired OAuth token), the resolved model, and the auth-independence
// contract (a nil/erroring Auth or Config never blocks the view). White-box
// (package tui) because statusView is unexported — the App-level "/status
// opens the panel" behavior is covered separately in command_test.go
// (package tui_test) through App's exported surface.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// statusOAuthExpires is the fixed reference expiry every OAuth fixture below
// renders against, so the "valid until <RFC3339>" text is deterministic
// across machines and CI (mirrors GoldenNow's role for the roster fixtures).
var statusOAuthExpires = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// statusTestEnv returns a CommandEnv fixed to GoldenCommandEnv's
// version/cwd/root identity, with Auth/Config swapped for closures a test
// controls directly — no real files touched, so the golden renders stay
// deterministic.
func statusTestEnv(auths []ProviderAuth, authErr error) CommandEnv {
	env := GoldenCommandEnv()
	env.Auth = func() ([]ProviderAuth, error) { return auths, authErr }
	return env
}

// fixtureStatusSession is the SessionInfo the attached/peeked-session tests
// render against — deliberately different from the default model so the
// Model row's "session override beats the default" precedence is visible.
func fixtureStatusSession() *SessionInfo {
	return &SessionInfo{
		ID:    "0192a1b2-stat-7000-8000-000000000001",
		Title: "wire the status view",
		Model: "claude-sonnet-5",
	}
}

func renderStatus(t *testing.T, name string, v statusView) {
	t.Helper()
	testkit.AssertGolden(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

func renderStatusStyled(t *testing.T, name string, v statusView) {
	t.Helper()
	testkit.AssertGoldenStyled(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

// TestGoldenStatusZeroAuth covers the auth-independence default: no
// providers authenticated collapses to one "not signed in" line (never a
// blank Providers row), and Model falls back to "—" with no session and no
// resolved default.
func TestGoldenStatusZeroAuth(t *testing.T) {
	v := statusView{theme: theme.Test(), env: statusTestEnv(nil, nil)}
	renderStatus(t, "status_zero_auth", v)
}

// TestGoldenStatusZeroAuthStyled is TestGoldenStatusZeroAuth's color-state
// counterpart: "not signed in" renders in WarnStyle, invisible under
// theme.Test()'s forced Ascii profile.
func TestGoldenStatusZeroAuthStyled(t *testing.T) {
	v := statusView{theme: testkit.ColorTheme(), env: statusTestEnv(nil, nil)}
	renderStatusStyled(t, "status_zero_auth", v)
}

// TestGoldenStatusOneProviderOAuth covers a single authenticated provider: a
// live (non-expired) OAuth credential renders "OAuth (valid until …)".
func TestGoldenStatusOneProviderOAuth(t *testing.T) {
	auths := []ProviderAuth{
		{Provider: "anthropic", Kind: KindOAuth, Expires: statusOAuthExpires},
	}
	v := statusView{theme: theme.Test(), env: statusTestEnv(auths, nil), defaultModel: "fable-5"}
	renderStatus(t, "status_one_provider_oauth", v)
}

// TestGoldenStatusMultipleProviders covers gofer's two-provider ceiling
// (runner.SupportedProviders): an expired OAuth token next to a plain API
// key, given out of alphabetical order to prove providerLines sorts by
// provider name for a stable render.
func TestGoldenStatusMultipleProviders(t *testing.T) {
	auths := []ProviderAuth{
		{Provider: "openai", Kind: KindAPIKey},
		{Provider: "anthropic", Kind: KindOAuth, Expires: statusOAuthExpires.Add(-24 * time.Hour), Expired: true},
	}
	v := statusView{theme: theme.Test(), env: statusTestEnv(auths, nil)}
	renderStatus(t, "status_multiple_providers", v)
}

// TestGoldenStatusMultipleProvidersStyled is TestGoldenStatusMultipleProviders'
// color-state counterpart: the expired anthropic entry renders in DangerStyle,
// the live openai API key in OKStyle.
func TestGoldenStatusMultipleProvidersStyled(t *testing.T) {
	auths := []ProviderAuth{
		{Provider: "openai", Kind: KindAPIKey},
		{Provider: "anthropic", Kind: KindOAuth, Expires: statusOAuthExpires.Add(-24 * time.Hour), Expired: true},
	}
	v := statusView{theme: testkit.ColorTheme(), env: statusTestEnv(auths, nil)}
	renderStatusStyled(t, "status_multiple_providers", v)
}

// TestGoldenStatusOverview covers the no-active-session case: Session name
// and Session ID both read "—", and Model falls back to defaultModel (the
// roster header's resolved default) since no session overrides it.
func TestGoldenStatusOverview(t *testing.T) {
	v := statusView{theme: theme.Test(), env: statusTestEnv(nil, nil), defaultModel: "fable-5"}
	renderStatus(t, "status_overview", v)
}

// TestGoldenStatusAttached covers the peeked/attached case: Session
// name/ID come from sess, and Model shows the session's own override rather
// than falling back to defaultModel.
func TestGoldenStatusAttached(t *testing.T) {
	v := statusView{
		theme:        theme.Test(),
		env:          statusTestEnv(nil, nil),
		sess:         fixtureStatusSession(),
		defaultModel: "fable-5",
	}
	renderStatus(t, "status_attached", v)
}

// TestStatusZeroCommandEnvDoesNotPanic covers the zero CommandEnv (nil
// Auth/Config closures) — the state a caller gets if it forgets to wire one
// — rendering "not signed in" rather than panicking.
func TestStatusZeroCommandEnvDoesNotPanic(t *testing.T) {
	v := statusView{theme: theme.Test()}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "not signed in") {
		t.Fatalf("expected the zero CommandEnv to render \"not signed in\", got:\n%s", got)
	}
}

// TestStatusAuthErrorTreatedAsNotSignedIn covers auth-independence's
// non-fatal contract: a read error from env.Auth renders exactly like zero
// providers, never blocking the view.
func TestStatusAuthErrorTreatedAsNotSignedIn(t *testing.T) {
	v := statusView{theme: theme.Test(), env: statusTestEnv(nil, errors.New("boom"))}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "not signed in") {
		t.Fatalf("expected an Auth error to render as not-signed-in, got:\n%s", got)
	}
}

// TestStatusSettingSourcesLineOmittedWhenAbsent covers "omit rather than
// blank-fill": with neither config layer present on disk (the
// GoldenCommandEnv fixture's Cwd/Root are fixed strings, not real
// directories), the Config row doesn't render at all.
func TestStatusSettingSourcesLineOmittedWhenAbsent(t *testing.T) {
	v := statusView{theme: theme.Test(), env: GoldenCommandEnv()}
	got := v.View(testkit.Width, testkit.Height)
	if strings.Contains(got, "Config:") {
		t.Fatalf("expected no Config row when neither layer exists on disk, got:\n%s", got)
	}
}

// TestStatusSettingSourcesLineListsPresentLayers covers the positive case
// with real temp-dir files: both the project (`<cwd>/.gofer/config.json`)
// and root (`<root>/config.json`) layers present are both named.
func TestStatusSettingSourcesLineListsPresentLayers(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cwd, ".gofer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".gofer", config.ConfigFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.DefaultPath(root), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := CommandEnv{
		Cwd:    cwd,
		Root:   root,
		Config: func() (config.Config, error) { return config.Load(config.DefaultPath(root)) },
	}
	v := statusView{theme: theme.Test(), env: env}
	got := v.View(200, testkit.Height)
	if !strings.Contains(got, "Config: project, ~/.gofer") {
		t.Fatalf("expected both config layers listed, got:\n%s", got)
	}
}
