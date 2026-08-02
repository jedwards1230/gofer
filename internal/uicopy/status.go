package uicopy

// status.go holds the /status command-panel tab's row labels: one function per
// row, each returning the finished "Label: value" line, so a label can never be
// pasted onto the wrong value.

// StatusVersion is the Version row.
func StatusVersion(v string) string { return "Version: " + v }

// StatusSession is the session-name row.
func StatusSession(name string) string { return "Session: " + name }

// StatusSessionID is the session-id row.
func StatusSessionID(id string) string { return "Session ID: " + id }

// StatusCwd is the working-directory row.
func StatusCwd(cwd string) string { return "Cwd: " + cwd }

// StatusModel is the resolved-model row.
func StatusModel(model string) string { return "Model: " + model }

// StatusProvidersHeader heads the Providers row when there is at least one
// authenticated provider to list beneath it.
const StatusProvidersHeader = "Providers:"

// StatusNoProviders is the Providers row when nothing is authenticated.
const StatusNoProviders = "Providers: not signed in"

// StatusAuthAPIKey labels an API-key credential.
const StatusAuthAPIKey = "API key"

// StatusAuthOAuth labels an OAuth credential that carries no expiry, which is
// bare on purpose: an unqualified label must not imply a validity window the
// credential never stated.
const StatusAuthOAuth = "OAuth"

// StatusAuthOAuthExpired labels an OAuth credential past its expiry.
const StatusAuthOAuthExpired = "OAuth (expired)"

// StatusAuthOAuthValidUntil labels a live OAuth credential with its expiry.
func StatusAuthOAuthValidUntil(expires string) string {
	return "OAuth (valid until " + expires + ")"
}

// StatusConfig is the settings-sources row naming which config layers exist.
func StatusConfig(layers string) string { return "Config: " + layers }

// StatusConfigUnreadable is appended to [StatusConfig] when the root config
// exists but cannot be read.
const StatusConfigUnreadable = " (unreadable)"
