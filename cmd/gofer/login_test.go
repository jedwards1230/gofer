package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/auth"
)

// fakeTokenDoer satisfies the SDK's unexported httpDoer interface
// structurally: it returns a canned Anthropic-shaped token response for any
// request, so the OAuth exchange never touches the network.
type fakeTokenDoer struct{}

func (fakeTokenDoer) Do(*http.Request) (*http.Response, error) {
	body := `{"access_token":"sk-ant-oat-fake","refresh_token":"ref","expires_in":3600}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// wantStatusTable builds the same aligned table writeStatusTable would, from
// an independently written copy of its layout rules, so the assertion isn't
// tautologically comparing production code against itself.
func wantStatusTable(rows [][3]string) string {
	var providerW, kindW int
	for _, r := range rows {
		if len(r[0]) > providerW {
			providerW = len(r[0])
		}
		if len(r[1]) > kindW {
			kindW = len(r[1])
		}
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %-*s  %s\n", providerW, r[0], kindW, r[1], r[2])
	}
	return b.String()
}

func TestAuthStatusFormatting(t *testing.T) {
	root := t.TempDir()
	store, err := auth.New(auth.WithRoot(root))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	if err := store.SetAPIKey("anthropic", "sk-ant-api-fake"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	now := time.Now()
	validExpires := now.Add(24 * time.Hour)
	if err := store.Set("openai", auth.Entry{
		Kind:    auth.KindOAuth,
		Access:  "access-valid",
		Refresh: "refresh-valid",
		Expires: validExpires.Unix(),
	}); err != nil {
		t.Fatalf("Set openai: %v", err)
	}

	expiredExpires := now.Add(-24 * time.Hour)
	if err := store.Set("azure", auth.Entry{
		Kind:    auth.KindOAuth,
		Access:  "access-expired",
		Refresh: "refresh-expired",
		Expires: expiredExpires.Unix(),
	}); err != nil {
		t.Fatalf("Set azure: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runAuth([]string{"--root", root}, &stdout, &stderr); err != nil {
		t.Fatalf("runAuth: %v", err)
	}

	want := wantStatusTable([][3]string{
		{"PROVIDER", "KIND", "STATUS"},
		{"anthropic", "api_key", "-"},
		{"azure", "oauth", "expired"},
		{"openai", "oauth", "valid until " + time.Unix(validExpires.Unix(), 0).Format(time.RFC3339)},
	})
	if got := stdout.String(); got != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestAuthStatusEmpty(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := runAuth([]string{"--root", root}, &stdout, &stderr); err != nil {
		t.Fatalf("runAuth: %v", err)
	}
	if got, want := stdout.String(), "No credentials configured.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestLogout(t *testing.T) {
	root := t.TempDir()
	store, err := auth.New(auth.WithRoot(root))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if err := store.SetAPIKey("openai", "sk-fake"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runLogout([]string{"--root", root, "openai"}, &stdout, &stderr); err != nil {
		t.Fatalf("runLogout: %v", err)
	}
	if got, want := stdout.String(), "Logged out of openai.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	entries, err := store.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, e := range entries {
		if e.Provider == "openai" {
			t.Fatalf("openai still present after logout: %+v", entries)
		}
	}

	// Logging out an absent provider is not an error.
	stdout.Reset()
	if err := runLogout([]string{"--root", root, "openai"}, &stdout, &stderr); err != nil {
		t.Fatalf("runLogout (already logged out): %v", err)
	}
}

func TestLoginAPIKey(t *testing.T) {
	root := t.TempDir()
	stdin := strings.NewReader("sk-test-key-123\n")
	var stdout, stderr bytes.Buffer
	err := runLogin(context.Background(), []string{"--root", root, "--api-key", "anthropic"}, stdin, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runLogin: %v", err)
	}
	if got, want := stdout.String(), "Stored API key for anthropic.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	store, err := auth.New(auth.WithRoot(root))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e, ok, err := store.Get("anthropic")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("no entry persisted")
	}
	if e.Kind != auth.KindAPIKey || e.Access != "sk-test-key-123" {
		t.Fatalf("unexpected entry: %+v", e)
	}
}

func TestLoginAPIKeyEmpty(t *testing.T) {
	root := t.TempDir()
	stdin := strings.NewReader("\n")
	var stdout, stderr bytes.Buffer
	err := runLogin(context.Background(), []string{"--root", root, "--api-key", "anthropic"}, stdin, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected error for empty api key")
	}
}

func TestLoginUnknownProvider(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runLogin(context.Background(), []string{"--root", root, "bogus"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected error for unknown provider")
	}
	var uerr *usageError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected *usageError, got %T: %v", err, err)
	}
}

// TestLoginNoProvider asserts `gofer login` with no provider argument is not
// a usage error: it prints a short provider listing (a login screen) and
// exits cleanly, since "how do I log in" is the natural first stop.
func TestLoginNoProvider(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runLogin(context.Background(), []string{"--root", root}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("runLogin with no provider: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "anthropic") || !strings.Contains(out, "openai") {
		t.Fatalf("provider listing missing a provider: %q", out)
	}
}

// TestLoginOAuthManualRedeem drives the real auth.Store's manual-code (paste)
// login for anthropic, with a fake HTTP client standing in for the token
// endpoint. It exercises the actual Store.Login + Login.Redeem path — the
// only fake in this test is the network.
func TestLoginOAuthManualRedeem(t *testing.T) {
	root := t.TempDir()
	store, err := auth.New(auth.WithRoot(root), auth.WithHTTPClient(fakeTokenDoer{}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	// A bare pasted code (no "#state" suffix) lets the flow fall back to its
	// own generated PKCE state, so the test doesn't need to scrape the
	// authorize URL for the real (randomly generated) state value.
	stdin := strings.NewReader("fakecode-from-browser\n")
	var stdout, stderr bytes.Buffer
	if err := loginWithOAuth(context.Background(), store, "anthropic", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("loginWithOAuth: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Open this URL in a browser to authorize:") {
		t.Fatalf("missing authorize URL prompt: %q", out)
	}
	if !strings.Contains(out, "Paste the code shown after authorizing:") {
		t.Fatalf("missing paste prompt: %q", out)
	}
	if !strings.Contains(out, "Logged in to anthropic.") {
		t.Fatalf("missing success message: %q", out)
	}

	e, ok, err := store.Get("anthropic")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("no entry persisted")
	}
	if e.Kind != auth.KindOAuth || e.Access != "sk-ant-oat-fake" || e.Refresh != "ref" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e.Expires == 0 {
		t.Fatalf("expiry not recorded")
	}

	entries, err := store.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	found := false
	for _, se := range entries {
		if se.Provider == "anthropic" {
			found = true
			if se.Kind != auth.KindOAuth || se.Expired {
				t.Fatalf("unexpected status entry: %+v", se)
			}
		}
	}
	if !found {
		t.Fatalf("anthropic missing from status: %+v", entries)
	}
}

func TestRunDispatchLoginLogoutAuth(t *testing.T) {
	root := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"login", "--root", root}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("login with no provider: exit = %d, want 0, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "anthropic") {
		t.Fatalf("login with no provider: stdout missing provider listing: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"login", "--root", root, "bogus"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("login with unknown provider: exit = %d, want 2", code)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"login", "--root", root, "--api-key", "openai"}, strings.NewReader("sk-fake\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("login --api-key: exit = %d, stderr = %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"auth", "--root", root}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("auth status: exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "openai") {
		t.Fatalf("auth status missing openai: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"logout", "--root", root, "openai"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("logout: exit = %d, stderr = %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"logout", "--root", root}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("logout with no provider: exit = %d, want 2", code)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"auth", "--root", root, "bogus"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("auth with bad subcommand: exit = %d, want 2", code)
	}
}

// TestFlagsAfterPositional guards the documented forms where flags follow the
// positional argument — Go's flag package stops at the first non-flag token,
// so these once failed with a usage error. Driven through run(...) so the full
// dispatch + interleaved-parse path is covered.
func TestFlagsAfterPositional(t *testing.T) {
	root := t.TempDir()

	// `login anthropic --api-key --root <tmp>`: flags entirely after the
	// positional must still store the key.
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"login", "anthropic", "--api-key", "--root", root},
		strings.NewReader("sk-after-positional\n"),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("login anthropic --api-key --root: exit = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "Stored API key for anthropic.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	store, err := auth.New(auth.WithRoot(root))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e, ok, err := store.Get("anthropic")
	if err != nil || !ok {
		t.Fatalf("Get anthropic: ok=%v err=%v", ok, err)
	}
	if e.Kind != auth.KindAPIKey || e.Access != "sk-after-positional" {
		t.Fatalf("unexpected entry: %+v", e)
	}

	// `auth status --root <tmp>`: flags after the `status` positional succeed.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"auth", "status", "--root", root}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("auth status --root: exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "anthropic") {
		t.Fatalf("auth status missing anthropic: %q", stdout.String())
	}

	// `logout anthropic --root <tmp>`: flag after the positional succeeds.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"logout", "anthropic", "--root", root}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("logout anthropic --root: exit = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "Logged out of anthropic.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// scriptedTokenDoer returns a pre-scripted HTTP status for each call and counts
// how many times the token endpoint is actually contacted — so a test can
// assert both retry behavior and that a locally-rejected paste never hits the
// network. A status past the end of the script defaults to 200.
type scriptedTokenDoer struct {
	statuses []int
	calls    int
}

func (d *scriptedTokenDoer) Do(*http.Request) (*http.Response, error) {
	status := http.StatusOK
	if d.calls < len(d.statuses) {
		status = d.statuses[d.calls]
	}
	d.calls++
	body := `{"access_token":"sk-ant-oat-fake","refresh_token":"ref","expires_in":3600}`
	if status != http.StatusOK {
		body = `{"error":"invalid_grant","error_description":"authorization code is invalid"}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// TestLoginRetry_ShapeRejectionNoEndpointContact asserts a paste that can't be
// an authorization code (whitespace — e.g. a fat-fingered shell command) is
// rejected LOCALLY: the token endpoint is never contacted, and the flow
// re-prompts rather than dying.
func TestLoginRetry_ShapeRejectionNoEndpointContact(t *testing.T) {
	root := t.TempDir()
	doer := &scriptedTokenDoer{} // would 200 if ever called
	store, err := auth.New(auth.WithRoot(root), auth.WithHTTPClient(doer))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	// Three whitespace pastes → exhausted, never a token-endpoint round trip.
	stdin := strings.NewReader("gofer login anthropic\nnot a code\nstill wrong\n")
	var stdout, stderr bytes.Buffer
	if err := loginWithOAuth(context.Background(), store, "anthropic", stdin, &stdout, &stderr); err == nil {
		t.Fatal("loginWithOAuth: got nil error, want exhausted after bad-shape pastes")
	}
	if doer.calls != 0 {
		t.Errorf("token endpoint contacted %d time(s), want 0 (shape rejected before the endpoint)", doer.calls)
	}
	if !strings.Contains(stderr.String(), "doesn't look like an authorization code") {
		t.Errorf("stderr = %q, want the shape-rejection hint", stderr.String())
	}
}

// TestLoginRetry_RetryThenSuccess asserts a first paste the endpoint rejects
// (400 invalid_grant) followed by a good code succeeds — one bad paste doesn't
// kill the flow, and the PKCE verifier is reused across the retry.
func TestLoginRetry_RetryThenSuccess(t *testing.T) {
	root := t.TempDir()
	doer := &scriptedTokenDoer{statuses: []int{http.StatusBadRequest, http.StatusOK}}
	store, err := auth.New(auth.WithRoot(root), auth.WithHTTPClient(doer))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	stdin := strings.NewReader("firstcode\nsecondcode\n")
	var stdout, stderr bytes.Buffer
	if err := loginWithOAuth(context.Background(), store, "anthropic", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("loginWithOAuth: %v", err)
	}
	if doer.calls != 2 {
		t.Errorf("token endpoint contacted %d time(s), want 2 (reject then success)", doer.calls)
	}
	if !strings.Contains(stderr.String(), "code rejected") {
		t.Errorf("stderr = %q, want the rejection retry hint", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Logged in to anthropic.") {
		t.Errorf("stdout = %q, want the success message", stdout.String())
	}
	if _, ok, _ := store.Get("anthropic"); !ok {
		t.Error("credential not persisted after retry-then-success")
	}
}

// TestLoginRetry_Exhausted asserts that after maxCodeAttempts rejected codes
// the login fails cleanly (no infinite loop), having contacted the endpoint
// exactly that many times.
func TestLoginRetry_Exhausted(t *testing.T) {
	root := t.TempDir()
	doer := &scriptedTokenDoer{statuses: []int{http.StatusBadRequest, http.StatusBadRequest, http.StatusBadRequest}}
	store, err := auth.New(auth.WithRoot(root), auth.WithHTTPClient(doer))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	stdin := strings.NewReader("codeone\ncodetwo\ncodethree\n")
	var stdout, stderr bytes.Buffer
	err = loginWithOAuth(context.Background(), store, "anthropic", stdin, &stdout, &stderr)
	if err == nil {
		t.Fatal("loginWithOAuth: got nil error, want failure after exhausted attempts")
	}
	if doer.calls != maxCodeAttempts {
		t.Errorf("token endpoint contacted %d time(s), want %d", doer.calls, maxCodeAttempts)
	}
	if !strings.Contains(err.Error(), "after 3 attempt") {
		t.Errorf("error = %v, want it to note attempts exhausted", err)
	}
	if _, ok, _ := store.Get("anthropic"); ok {
		t.Error("credential unexpectedly persisted after all attempts failed")
	}
}
