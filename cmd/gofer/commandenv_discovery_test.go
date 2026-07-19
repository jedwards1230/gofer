package main

// commandenv_discovery_test.go guards ONE thing: that the CommandEnv the roster
// TUI actually runs with performs live model discovery.
//
// It exists because that property fails silently. modelcatalog's discovery is
// opt-in (see modelcatalog.WithDiscovery's doc for why that default is the safe
// one), which means a buildCommandEnv that stops passing WithDiscovery still
// compiles, still returns a complete and correct-looking model list — the
// compiled-in floor — and still passes every other test in this repo. The
// feature just quietly stops existing. modelcatalog's own tests cannot see it
// either: they pass WithDiscovery themselves, so they prove Catalog CAN
// discover when asked, never that gofer asks.
//
// No vendor host is reachable from here. Both modelDiscoveryClient and
// modelDiscoveryBaseURL are pinned to an httptest server, and the pinned client
// covers the SDK auth store's token refresh as well as the listing itself —
// modelcatalog threads the injected client into auth.WithHTTPClient precisely
// so the two cannot be pinned separately (see its codexCredential doc).
//
// Names here are deliberately file-scoped (envDiscovery* / tuiEnv*) because
// cmd/gofer is edited by several workstreams at once and a second file guarding
// the same property would otherwise collide on helper names.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/auth"
)

// envDiscoveryFloorIDs is the compiled-in Codex floor — exactly what a call
// site that FORGOT WithDiscovery returns. Every assertion below is chosen so
// that the floor and the live listing cannot both satisfy it.
var envDiscoveryFloorIDs = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.3-codex-spark"}

// envDiscoveryLiveBody is a listing deliberately unlike the floor: it carries an
// id the floor does not have, drops ids the floor does, and includes a "hide"
// entry that must be filtered out.
const envDiscoveryLiveBody = `{"models":[
	{"slug":"gpt-5.9-proof","display_name":"GPT-5.9 Proof","context_window":512000,"visibility":"list"},
	{"slug":"gpt-5.6-terra","display_name":"GPT-5.6 Terra (live)","context_window":272000,"visibility":"list"},
	{"slug":"codex-auto-review","display_name":"Codex Auto Review","visibility":"hide"}
]}`

// TestCommandEnvModelsPerformsLiveDiscovery is the must-fire twin. It calls
// buildCommandEnv — the real production constructor, with no discovery options
// threaded in by the test — and asserts its Models closure reached the network
// and returned what the listing said.
//
// If buildCommandEnv stops passing WithDiscovery, no request is issued, the
// floor comes back instead, and both assertions below fail.
func TestCommandEnvModelsPerformsLiveDiscovery(t *testing.T) {
	root := tuiEnvOAuthRoot(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Errorf("listing request carried no client_version parameter")
		}
		if got := r.Header.Get("Authorization"); got == "" {
			t.Errorf("listing request carried no Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(envDiscoveryLiveBody))
	}))
	defer srv.Close()
	pinEnvDiscovery(t, srv)

	env := buildCommandEnv(root, t.TempDir())
	if env.Models == nil {
		t.Fatal("buildCommandEnv left CommandEnv.Models nil — the picker would never load a live catalog")
	}

	models, err := env.Models(context.Background(), "openai")
	if err != nil {
		t.Fatalf("env.Models: %v", err)
	}

	if hits == 0 {
		t.Fatal("the production CommandEnv issued no listing request — buildCommandEnv is not passing modelcatalog.WithDiscovery, so /model silently shows only the compiled-in floor")
	}

	var got []string
	for _, m := range models {
		got = append(got, m.ID)
	}
	if !slices.Contains(got, "gpt-5.9-proof") {
		t.Fatalf("env.Models = %v, want the live-only id gpt-5.9-proof — the result did not come from the listing", got)
	}
	if slices.Equal(got, envDiscoveryFloorIDs) {
		t.Fatalf("env.Models returned the compiled-in floor %v — discovery did not reach the result", got)
	}
	// The vendor's "hide" entries are not pickable models.
	if slices.Contains(got, "codex-auto-review") {
		t.Errorf("env.Models = %v, want hidden listing entries filtered out", got)
	}
}

// TestCommandEnvModelsDefaultsToTheRealTransport closes the gap the twin above
// cannot cover on its own. That test PINS modelDiscoveryClient, so it would
// keep passing even if the production defaults were changed to something inert.
// This asserts the unoverridden defaults are the real ones — nil/"" being the
// documented "http.DefaultClient against the vendor host", and http.DefaultClient
// being that same thing named explicitly.
//
// It makes no request: it only inspects the defaults, so nothing here can leave
// the machine.
func TestCommandEnvModelsDefaultsToTheRealTransport(t *testing.T) {
	if modelDiscoveryClient != http.DefaultClient {
		t.Errorf("modelDiscoveryClient = %v, want http.DefaultClient — production discovery must run on the real transport", modelDiscoveryClient)
	}
	if modelDiscoveryBaseURL != "" {
		t.Errorf("modelDiscoveryBaseURL = %q, want \"\" — an empty base URL is what selects the real vendor host", modelDiscoveryBaseURL)
	}
}

// TestCommandEnvModelsFallsBackToFloor pins the other half of the contract the
// picker depends on: discovery is an upgrade, never a dependency. A vendor
// returning 500 must still yield a usable list, because the picker replaces its
// floor with whatever this returns.
func TestCommandEnvModelsFallsBackToFloor(t *testing.T) {
	root := tuiEnvOAuthRoot(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	pinEnvDiscovery(t, srv)

	models, err := buildCommandEnv(root, t.TempDir()).Models(context.Background(), "openai")
	if err != nil {
		t.Fatalf("env.Models on a failing listing = %v, want the floor and no error", err)
	}
	var got []string
	for _, m := range models {
		got = append(got, m.ID)
	}
	if !slices.Equal(got, envDiscoveryFloorIDs) {
		t.Fatalf("env.Models = %v, want the static Codex floor %v after a failed discovery", got, envDiscoveryFloorIDs)
	}
}

// pinEnvDiscovery points production discovery at srv and refuses every other
// host, restoring the real values afterwards. The transport is the standing
// guard: any request that is not to srv fails the test rather than leaving the
// machine.
func pinEnvDiscovery(t *testing.T, srv *httptest.Server) {
	t.Helper()
	allowed := srv.Listener.Addr().String()
	prevClient, prevURL := modelDiscoveryClient, modelDiscoveryBaseURL
	t.Cleanup(func() { modelDiscoveryClient, modelDiscoveryBaseURL = prevClient, prevURL })

	modelDiscoveryClient = &http.Client{Transport: tuiEnvRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != allowed {
			t.Errorf("blocked a request to a non-test host %q", r.URL.Host)
			return nil, errors.New("blocked: request to a non-test host " + r.URL.Host)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	modelDiscoveryBaseURL = srv.URL
}

// tuiEnvOAuthRoot returns a temp store root holding a non-expired OpenAI OAuth
// credential, with both provider env vars cleared so no ambient API key on the
// developer's machine can change which credential kind is resolved.
//
// Non-expired matters: it keeps the token-refresh path out of these tests
// entirely, so the only request they can produce is the listing.
func tuiEnvOAuthRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	root := t.TempDir()

	store, err := auth.New(auth.WithRoot(root))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	entry := auth.Entry{
		Kind:   auth.KindOAuth,
		Access: "tok-test",
		Extra:  map[string]string{"chatgpt_account_id": "acct-test"},
	}
	if err := store.Set("openai", entry); err != nil {
		t.Fatalf("store.Set(openai): %v", err)
	}
	return root
}

// tuiEnvRoundTripFunc adapts a function to http.RoundTripper.
type tuiEnvRoundTripFunc func(*http.Request) (*http.Response, error)

func (f tuiEnvRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
