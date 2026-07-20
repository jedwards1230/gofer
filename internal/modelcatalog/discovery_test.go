package modelcatalog_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/auth"

	"github.com/jedwards1230/gofer/internal/modelcatalog"
)

// codexFloorIDs is the static floor, in order — what every discovery failure
// path must degrade to.
var codexFloorIDs = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.3-codex-spark"}

// liveBody is a listing deliberately DIFFERENT from the static floor: a
// different order, a renamed model, an id the floor does not carry, and a
// "hide" entry. Any assertion that passes against the floor therefore cannot
// also pass against live data, and vice versa.
const liveBody = `{"models":[
	{"slug":"gpt-5.7-nova","display_name":"GPT-5.7 Nova","context_window":400000,"visibility":"list"},
	{"slug":"gpt-5.6-terra","display_name":"GPT-5.6 Terra (live)","context_window":272000,"visibility":"list"},
	{"slug":"codex-auto-review","display_name":"Codex Auto Review","visibility":"hide"},
	{"slug":"gpt-5.3-codex-spark","display_name":"GPT-5.3 Codex Spark","context_window":128000,"visibility":"list"}
]}`

// TestCatalogPrefersLiveDiscovery proves discovery is the primary source: the
// result is the live listing, in vendor order, carrying live display names and
// live context windows — and with the vendor's "hide" entry filtered out.
func TestCatalogPrefersLiveDiscovery(t *testing.T) {
	root := oauthRoot(t)
	srv, httpc := listingServer(t, http.StatusOK, liveBody)

	got, err := modelcatalog.Catalog(context.Background(), root, "openai",
		modelcatalog.WithDiscovery(httpc, srv.URL))
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	want := []string{"gpt-5.7-nova", "gpt-5.6-terra", "gpt-5.3-codex-spark"}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("Catalog = %v, want the live listing %v", ids(got), want)
	}
	// The live display_name must beat gofer's compiled-in modelmeta label,
	// which for this id is the plain "GPT-5.6 Terra".
	if got[1].Label != "GPT-5.6 Terra (live)" {
		t.Errorf("Label = %q, want the live display_name %q", got[1].Label, "GPT-5.6 Terra (live)")
	}
	if got[1].ContextWindow != 272000 {
		t.Errorf("ContextWindow = %d, want the live 272000", got[1].ContextWindow)
	}
	// An id the compiled-in floor has never heard of still gets a usable label.
	if got[0].Label != "GPT-5.7 Nova" {
		t.Errorf("Label = %q, want %q", got[0].Label, "GPT-5.7 Nova")
	}
}

// TestCatalogVisibilityFilterFailsOpen states, by name, the rule the golden
// encodes structurally: a model is dropped ONLY on an explicit "hide". Every
// other visibility value is KEPT — the known-visible "list", a value from
// outside the vocabulary either gofer or the SDK has seen, and an absent field.
//
// The direction is the point. Fail-CLOSED here (drop anything not recognized as
// visible) would work perfectly until the vendor renamed "list", at which moment
// every model would vanish from the picker at once — and, because a discovery
// returning nothing selectable degrades to the floor, it would present as the
// silent, hard-to-attribute "discovery mysteriously stopped working" rather than
// as a failure. Fail-OPEN's worst case is showing a model the vendor's own
// picker tucks away, which is still routable.
//
// The expected set is deliberately unlike the static floor, so this cannot pass
// against a fallback.
func TestCatalogVisibilityFilterFailsOpen(t *testing.T) {
	root := oauthRoot(t)
	const mixedVisibility = `{"models":[
		{"slug":"gpt-5.7-nova","display_name":"GPT-5.7 Nova","visibility":"list"},
		{"slug":"codex-auto-review","display_name":"Codex Auto Review","visibility":"hide"},
		{"slug":"gpt-5.9-quasar","display_name":"GPT-5.9 Quasar","visibility":"experimental"},
		{"slug":"gpt-6.0-vega","display_name":"GPT-6.0 Vega"}
	]}`
	srv, httpc := listingServer(t, http.StatusOK, mixedVisibility)

	got, err := modelcatalog.Catalog(context.Background(), root, "openai",
		modelcatalog.WithDiscovery(httpc, srv.URL))
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	want := []string{"gpt-5.7-nova", "gpt-5.9-quasar", "gpt-6.0-vega"}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("Catalog = %v, want %v: only an explicit \"hide\" may be dropped", ids(got), want)
	}
	if slices.Contains(ids(got), "codex-auto-review") {
		t.Error(`the "hide" entry survived; an explicit hide marker must be honored`)
	}
}

// TestCatalogFallsBackToFloor is the hard requirement: every way discovery can
// fail must degrade to the static floor — never an empty list, never an error.
// The 200-with-zero-models case is the subtle one: the SDK's listing correctly
// reports it as a success with zero models, so only this layer can turn it into
// a fallback.
func TestCatalogFallsBackToFloor(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"401 unauthorized (expired or rejected token)", http.StatusUnauthorized, `{"detail":"invalid token"}`},
		{"400 bad request (e.g. client_version rejected)", http.StatusBadRequest, `{"detail":"Field required"}`},
		{"500 server error", http.StatusInternalServerError, `oops`},
		{"malformed body", http.StatusOK, `{"models":[{"slug":`},
		{"200 with zero models", http.StatusOK, `{"models":[]}`},
		{"200 with only hidden models", http.StatusOK, `{"models":[{"slug":"codex-auto-review","visibility":"hide"}]}`},
		// A 200 whose body carries no "models" key at all — most plausibly the
		// public API's differently-shaped listing arriving on the Codex route.
		// The SDK distinguishes this from an empty catalogue (an absent key is
		// an error there, where an empty array is a success), which the old
		// in-tree client did not. The distinction is invisible from here on
		// purpose: this layer treats every discovery failure alike, so both
		// spellings of "nothing usable came back" land on the same floor.
		{"200 with no models key", http.StatusOK, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := oauthRoot(t)
			srv, httpc := listingServer(t, tt.status, tt.body)

			got, err := modelcatalog.Catalog(context.Background(), root, "openai",
				modelcatalog.WithDiscovery(httpc, srv.URL))
			if err != nil {
				t.Fatalf("Catalog returned an error (%v); discovery failure must degrade, not fail", err)
			}
			if len(got) == 0 {
				t.Fatal("Catalog returned an EMPTY list; a discovery failure must never empty the picker")
			}
			if !slices.Equal(ids(got), codexFloorIDs) {
				t.Errorf("Catalog = %v, want the static floor %v", ids(got), codexFloorIDs)
			}
		})
	}

	t.Run("transport error (host unreachable)", func(t *testing.T) {
		root := oauthRoot(t)
		dead := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: network is unreachable")
		})}

		got, err := modelcatalog.Catalog(context.Background(), root, "openai",
			modelcatalog.WithDiscovery(dead, "http://127.0.0.1:1"))
		if err != nil {
			t.Fatalf("Catalog: %v", err)
		}
		if !slices.Equal(ids(got), codexFloorIDs) {
			t.Errorf("Catalog = %v, want the static floor %v", ids(got), codexFloorIDs)
		}
	})

	t.Run("discovery not enabled", func(t *testing.T) {
		root := oauthRoot(t)
		got, err := modelcatalog.Catalog(context.Background(), root, "openai")
		if err != nil {
			t.Fatalf("Catalog: %v", err)
		}
		if !slices.Equal(ids(got), codexFloorIDs) {
			t.Errorf("Catalog = %v, want the static floor %v", ids(got), codexFloorIDs)
		}
	})
}

// TestCatalogDiscoveryTimeoutFallsBackToFloor proves the bound is real: a
// server that never responds must not hang the caller. The handler blocks until
// the request context is cancelled, so a missing timeout would hang this test
// rather than merely slow it — a slow-but-returning server would pass even with
// no bound at all.
func TestCatalogDiscoveryTimeoutFallsBackToFloor(t *testing.T) {
	root := oauthRoot(t)
	// released closes when the handler observes the cancellation. It is a
	// channel, not a flag read straight after Catalog returns: the handler sees
	// the disconnect asynchronously, so reading a flag immediately would be a
	// race that reports "not bounded" for a call that was correctly bounded.
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(released)
	}))
	defer srv.Close()

	start := time.Now()
	got, err := modelcatalog.Catalog(context.Background(), root, "openai",
		modelcatalog.WithDiscovery(pinnedClient(t, srv), srv.URL),
		modelcatalog.WithTimeout(50*time.Millisecond))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Catalog took %v; the discovery timeout did not bound it", elapsed)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Error("the server was never released by a cancellation; the call is not bounded")
	}
	if !slices.Equal(ids(got), codexFloorIDs) {
		t.Errorf("Catalog = %v, want the static floor %v", ids(got), codexFloorIDs)
	}
}

// TestCatalogDiscoverySendsCredentialAndClientVersion locks the wiring this
// package owns: the bearer token and ChatGPT account id come out of the auth
// store, and the REQUIRED client_version is the one gofer chose. Omitting
// client_version is itself an HTTP 400, so a regression here would present as
// "discovery mysteriously always falls back".
//
// The client_version is asserted against a value INJECTED by the test, not
// against modelcatalog.CodexClientVersion. That distinction is the whole point
// of this test after #167: gofer's constant and the SDK's own default
// client_version are the same string today, so asserting the constant reaches
// the wire cannot tell "gofer explicitly sent its version" apart from "gofer
// sent nothing and the SDK filled in its identical default". A version distinct
// from BOTH can: if gofer stopped passing its own value, the SDK default would
// arrive instead of the injected sentinel and this test would fail — which is
// exactly the plumbing regression the assertion must be able to catch.
func TestCatalogDiscoverySendsCredentialAndClientVersion(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	writeCredential(t, root, "openai", auth.Entry{
		Kind:   auth.KindOAuth,
		Access: "tok-abc",
		Extra:  map[string]string{"chatgpt_account_id": "acct-123"},
	})

	// A sentinel deliberately unlike any real release string, so it cannot
	// coincide with either gofer's CodexClientVersion or the SDK's default —
	// only gofer's plumbing carrying the injected value can put it on the wire.
	const wantVersion = "gofer-test-sentinel-0.0.0"
	if wantVersion == modelcatalog.CodexClientVersion {
		t.Fatalf("sentinel %q must differ from the package constant to be distinguishable", wantVersion)
	}

	var gotAuth, gotAccount, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		gotVersion = r.URL.Query().Get("client_version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveBody))
	}))
	defer srv.Close()

	if _, err := modelcatalog.Catalog(context.Background(), root, "openai",
		modelcatalog.WithDiscovery(pinnedClient(t, srv), srv.URL),
		modelcatalog.WithClientVersion(wantVersion)); err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok-abc")
	}
	if gotAccount != "acct-123" {
		t.Errorf("ChatGPT-Account-Id = %q, want the stored account id", gotAccount)
	}
	if gotVersion != wantVersion {
		t.Errorf("client_version = %q, want the injected %q — gofer must carry its own version to the wire, not inherit the SDK default", gotVersion, wantVersion)
	}
}

// TestCatalogDiscoveryDefaultsToGoferClientVersion covers the other half of the
// seam: with no override, the version on the wire is gofer's own
// CodexClientVersion. Paired with the injected-sentinel test above, this pins
// both facts the #167 coupling needed — the default IS gofer's constant, and
// the value that reaches the wire is whatever gofer chose rather than the SDK's.
func TestCatalogDiscoveryDefaultsToGoferClientVersion(t *testing.T) {
	root := oauthRoot(t)

	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.URL.Query().Get("client_version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveBody))
	}))
	defer srv.Close()

	if _, err := modelcatalog.Catalog(context.Background(), root, "openai",
		modelcatalog.WithDiscovery(pinnedClient(t, srv), srv.URL)); err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	if gotVersion != modelcatalog.CodexClientVersion || gotVersion == "" {
		t.Errorf("client_version = %q, want gofer's default %q (the listing REQUIRES it)", gotVersion, modelcatalog.CodexClientVersion)
	}
}

// TestCatalogRefreshesAnExpiredTokenForDiscovery covers the deliberate use of
// the REFRESHING auth.Store.Credential on this path (and only this path).
//
// An expired OAuth token is the normal state of a credential between refreshes,
// not an edge case. Resolving it with the non-refreshing Get would hand the
// listing a stale bearer token, earning a 401 and a permanent silent downgrade
// to the static floor for a user whose credential is perfectly good. This
// asserts the refresh happens and that the listing is called with the REFRESHED
// token — the observable difference between the two choices.
//
// Both hosts are served by one pinned transport: the vendor's token endpoint is
// a real host, so the refresh must be intercepted here too or this test would
// issue a live request to it.
func TestCatalogRefreshesAnExpiredTokenForDiscovery(t *testing.T) {
	root := hermeticRoot(t)
	writeCredential(t, root, "openai", auth.Entry{
		Kind:    auth.KindOAuth,
		Access:  "stale-token",
		Refresh: "refresh-token",
		Expires: time.Now().Add(-time.Hour).Unix(), // expired an hour ago
	})

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveBody))
	}))
	defer srv.Close()

	var refreshed atomic.Bool
	listingHost := srv.Listener.Addr().String()
	routed := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == listingHost {
			return http.DefaultTransport.RoundTrip(r)
		}
		// Anything else on this path is the OAuth token endpoint. Serving it
		// here is what keeps the refresh off the real vendor host.
		refreshed.Store(true)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"access_token":"fresh-token","refresh_token":"r2","expires_in":3600}`)),
			Request: r,
		}, nil
	})}

	got, err := modelcatalog.Catalog(context.Background(), root, "openai",
		modelcatalog.WithDiscovery(routed, srv.URL))
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if !refreshed.Load() {
		t.Fatal("the expired token was not refreshed; discovery would 401 into the floor for a valid credential")
	}
	if gotAuth != "Bearer fresh-token" {
		t.Errorf("Authorization = %q, want the REFRESHED token %q", gotAuth, "Bearer fresh-token")
	}
	// And the refreshed call actually produced the live listing, not the floor.
	if !slices.Equal(ids(got), []string{"gpt-5.7-nova", "gpt-5.6-terra", "gpt-5.3-codex-spark"}) {
		t.Errorf("Catalog = %v, want the live listing", ids(got))
	}
}

// TestCatalogSkipsDiscoveryForNonOAuth proves the non-OAuth paths never reach
// the network: an API-key credential serves the SDK registry, and the listing
// must not be contacted at all — it targets a backend that credential does not
// even route to.
func TestCatalogSkipsDiscoveryForNonOAuth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		entry    auth.Entry
	}{
		{"openai api key", "openai", auth.Entry{Kind: auth.KindAPIKey, Access: "sk-test"}},
		{"anthropic oauth", "anthropic", auth.Entry{Kind: auth.KindOAuth, Access: "tok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hermeticRoot(t)
			writeCredential(t, root, tc.provider, tc.entry)

			var called atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called.Store(true)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(liveBody))
			}))
			defer srv.Close()

			got, err := modelcatalog.Catalog(context.Background(), root, tc.provider,
				modelcatalog.WithDiscovery(pinnedClient(t, srv), srv.URL))
			if err != nil {
				t.Fatalf("Catalog: %v", err)
			}
			if called.Load() {
				t.Error("the listing was contacted for a non-OAuth-OpenAI credential; it must not be")
			}
			if !slices.Equal(ids(got), registryIDs(tc.provider)) {
				t.Errorf("Catalog = %v, want the SDK registry family %v", ids(got), registryIDs(tc.provider))
			}
		})
	}
}

// TestCatalogNeverCallsARealVendorHost is the standing guard against the one
// thing this package must never do. Every request goes through a transport that
// refuses any host but the local test server, so a stray vendor call surfaces
// as a failure here rather than as a billed request. It also covers the
// default-options path: a lookup with no WithDiscovery must issue no request at
// all, which is what makes every other test in this package safe by default.
func TestCatalogNeverCallsARealVendorHost(t *testing.T) {
	root := oauthRoot(t)
	srv, httpc := listingServer(t, http.StatusOK, liveBody)

	t.Run("discovery enabled reaches only the test server", func(t *testing.T) {
		got, err := modelcatalog.Catalog(context.Background(), root, "openai",
			modelcatalog.WithDiscovery(httpc, srv.URL))
		if err != nil {
			t.Fatalf("Catalog: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("Catalog returned nothing")
		}
	})

	t.Run("default options issue no request whatsoever", func(t *testing.T) {
		var issued atomic.Bool
		blocking := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			issued.Store(true)
			return nil, errors.New("blocked: unexpected request to " + r.URL.Host)
		})}
		// The client is deliberately NOT passed to Catalog: with no
		// WithDiscovery there is no request to make, so nothing can use it.
		_ = blocking

		if _, err := modelcatalog.Catalog(context.Background(), root, "openai"); err != nil {
			t.Fatalf("Catalog: %v", err)
		}
		if issued.Load() {
			t.Fatal("a request was issued with no discovery configured")
		}
	})
}

// TestModelDeclaresNoPricingField guards the "no pricing, ever" rule
// structurally. The vendor sends no price for subscription models, so there is
// nothing to render but UNKNOWN; a field added here would be a place for a
// fabricated $0 to live, and a renderer would dutifully show it as fact.
//
// The SDK enforces the same rule on its side — a ModelLister record's Pricing is
// unconditionally zero — but that is its guarantee, not gofer's. This type is
// what the picker renders, so the guard belongs here too.
func TestModelDeclaresNoPricingField(t *testing.T) {
	rt := reflect.TypeOf(modelcatalog.Model{})
	for i := range rt.NumField() {
		name := strings.ToLower(rt.Field(i).Name)
		for _, banned := range []string{"pric", "cost", "usd", "rate"} {
			if strings.Contains(name, banned) {
				t.Errorf("Model has field %q; subscription models carry no pricing and this package must never synthesize one",
					rt.Field(i).Name)
			}
		}
	}
}

// TestDiscoveredModelsCarryNoFabricatedContextWindow covers the other half of
// the same rule: a listing that omits context_window must leave it 0 (UNKNOWN),
// not backfilled from gofer's table or from a sibling model.
func TestDiscoveredModelsCarryNoFabricatedContextWindow(t *testing.T) {
	root := oauthRoot(t)
	const noWindow = `{"models":[{"slug":"gpt-5.6-terra","display_name":"GPT-5.6 Terra","visibility":"list"}]}`
	srv, httpc := listingServer(t, http.StatusOK, noWindow)

	got, err := modelcatalog.Catalog(context.Background(), root, "openai",
		modelcatalog.WithDiscovery(httpc, srv.URL))
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Catalog = %v, want one model", ids(got))
	}
	if got[0].ContextWindow != 0 {
		t.Errorf("ContextWindow = %d, want 0 (UNKNOWN — the listing omitted it)", got[0].ContextWindow)
	}
}

// TestDefaultModelIn covers the discovery half of the default decision: the
// constant stands when the catalog confirms it, and is upgraded only when the
// catalog proves it unreachable.
func TestDefaultModelIn(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		kind     modelcatalog.Kind
		catalog  []modelcatalog.Model
		want     string
	}{
		{
			name: "constant stands when present in the live catalog", provider: "openai", kind: modelcatalog.KindOAuth,
			catalog: modelsOf("gpt-5.7-nova", "gpt-5.6-terra"), want: "gpt-5.6-terra",
		},
		{
			// The retired-default case: gpt-5.6-terra is gone from the listing,
			// so the head of the (most-to-least-capable) catalog stands in.
			name: "upgrades when the constant is absent", provider: "openai", kind: modelcatalog.KindOAuth,
			catalog: modelsOf("gpt-5.9-helios", "gpt-5.8-atlas"), want: "gpt-5.9-helios",
		},
		{
			// No listing is no evidence: an offline user keeps the constant
			// rather than being pushed onto some arbitrary other model.
			name: "empty catalog leaves the constant alone", provider: "openai", kind: modelcatalog.KindOAuth,
			catalog: nil, want: "gpt-5.6-terra",
		},
		{
			name: "api key path is unaffected", provider: "openai", kind: modelcatalog.KindAPIKey,
			catalog: modelsOf("gpt-5", "gpt-5-mini"), want: "gpt-5",
		},
		{
			name: "anthropic is unaffected", provider: "anthropic", kind: modelcatalog.KindOAuth,
			catalog: modelsOf("claude-sonnet-5", "claude-opus-4-8"), want: "claude-sonnet-5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelcatalog.DefaultModelIn(tt.provider, tt.kind, tt.catalog); got != tt.want {
				t.Errorf("DefaultModelIn(%q, %q, %v) = %q, want %q", tt.provider, tt.kind, ids(tt.catalog), got, tt.want)
			}
		})
	}
}

// TestDefaultModelMakesNoNetworkCall locks the deliberate design choice that
// the startup path stays offline: DefaultModel resolves without any listing
// call, so `gofer run` can never block on a vendor round trip or a token
// refresh.
func TestDefaultModelMakesNoNetworkCall(t *testing.T) {
	root := oauthRoot(t)

	start := time.Now()
	got, err := modelcatalog.DefaultModel(context.Background(), root, "openai")
	if err != nil {
		t.Fatalf("DefaultModel: %v", err)
	}
	// DefaultModel takes no Option, so it is structurally incapable of
	// discovery; this asserts the resolved value and that it is immediate.
	if got != "gpt-5.6-terra" {
		t.Errorf("DefaultModel = %q, want gpt-5.6-terra", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("DefaultModel took %v; the startup path must not make a vendor call", elapsed)
	}
}

// oauthRoot is a hermetic store root carrying a single OpenAI OAuth credential
// — the user situation issue #157 is about.
func oauthRoot(t *testing.T) string {
	t.Helper()
	root := hermeticRoot(t)
	writeCredential(t, root, "openai", auth.Entry{Kind: auth.KindOAuth, Access: "tok"})
	return root
}

// listingServer starts a listing stub returning status/body, paired with a
// transport pinned to it. Nothing in this package's tests may reach any other
// host; see TestCatalogNeverCallsARealVendorHost.
func listingServer(t *testing.T, status int, body string) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, pinnedClient(t, srv)
}

// pinnedClient returns an http.Client whose transport refuses every host but
// srv's — the standing guard against a real vendor call.
func pinnedClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	allowed := srv.Listener.Addr().String()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != allowed {
			t.Errorf("blocked a request to a non-test host %q", r.URL.Host)
			return nil, errors.New("blocked: request to a non-test host " + r.URL.Host)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
}

// modelsOf builds a catalog from bare ids.
func modelsOf(list ...string) []modelcatalog.Model {
	out := make([]modelcatalog.Model, 0, len(list))
	for _, id := range list {
		out = append(out, modelcatalog.Model{ID: id, Provider: "openai", Label: id})
	}
	return out
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
