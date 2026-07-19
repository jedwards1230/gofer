package openaimodels

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// Test policy for this package:
//
//   - NO test may reach a real vendor host. Every client is built by
//     pinnedClient, whose transport refuses any host other than the httptest
//     server, and TestListPinnedTransportBlocksRealHost is the canary proving
//     that guard actually fires.
//   - Tokens are always dummies, never read from the environment, so a
//     developer's real credential cannot be picked up by a test run.

// errOffHost is returned by the pinned transport for any request addressed
// somewhere other than the test server.
var errOffHost = errors.New("test transport: refused request to non-test host")

// pinnedTransport fails any request whose host differs from allowHost.
type pinnedTransport struct {
	allowHost string
	base      http.RoundTripper
}

func (p pinnedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != p.allowHost {
		return nil, fmt.Errorf("%w: %s", errOffHost, r.URL.Host)
	}
	return p.base.RoundTrip(r)
}

// pinnedClient returns an HTTP client that can only reach serverURL's host.
func pinnedClient(t *testing.T, serverURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	return &http.Client{Transport: pinnedTransport{allowHost: u.Host, base: http.DefaultTransport}}
}

// testRequest is a fully-populated Request pointed at srv.
func testRequest(srv *httptest.Server) Request {
	return Request{
		BaseURL:       srv.URL,
		Token:         "test-oauth-token",
		AccountID:     "test-account-id",
		ClientVersion: "0.0.0-test",
	}
}

// catalogFixture mirrors the real response shape: a "models" array whose
// entries carry slug/display_name/description/context_window/visibility plus
// fields this package does not model (supported_reasoning_levels,
// available_in_plans). The visibility split matches the vendor's: five listed,
// three hidden.
const catalogFixture = `{
  "models": [
    {
      "slug": "gpt-5.6-sol",
      "display_name": "GPT-5.6-Sol",
      "description": "Latest frontier agentic coding model.",
      "context_window": 272000,
      "max_context_window": 272000,
      "visibility": "list",
      "supported_reasoning_levels": [
        {"effort": "low", "description": "Fastest"},
        {"effort": "medium", "description": "Balanced"},
        {"effort": "high", "description": "Most thorough"},
        {"effort": "xhigh", "description": "Extended thinking"}
      ],
      "available_in_plans": ["plus", "pro", "business"]
    },
    {
      "slug": "gpt-5.6-terra",
      "display_name": "GPT-5.6-Terra",
      "description": "Balanced agentic coding model.",
      "context_window": 272000,
      "max_context_window": 272000,
      "visibility": "list",
      "supported_reasoning_levels": [{"effort": "medium", "description": "Balanced"}],
      "available_in_plans": ["plus", "pro"]
    },
    {
      "slug": "gpt-5.6-luna",
      "display_name": "GPT-5.6-Luna",
      "description": "Fast agentic coding model.",
      "context_window": 272000,
      "max_context_window": 272000,
      "visibility": "list",
      "supported_reasoning_levels": [{"effort": "low", "description": "Fastest"}],
      "available_in_plans": ["plus", "pro"]
    },
    {
      "slug": "gpt-5.5",
      "display_name": "GPT-5.5",
      "description": "Previous frontier model.",
      "context_window": 272000,
      "max_context_window": 272000,
      "visibility": "list",
      "supported_reasoning_levels": [{"effort": "high", "description": "Most thorough"}],
      "available_in_plans": ["plus", "pro"]
    },
    {
      "slug": "gpt-5.3-codex-spark",
      "display_name": "GPT-5.3-Codex-Spark",
      "description": "Small, fast model for lightweight tasks.",
      "context_window": 128000,
      "max_context_window": 128000,
      "visibility": "list",
      "supported_reasoning_levels": [{"effort": "low", "description": "Fastest"}],
      "available_in_plans": ["plus", "pro"]
    },
    {
      "slug": "gpt-5.4",
      "display_name": "GPT-5.4",
      "description": "Superseded model.",
      "context_window": 272000,
      "max_context_window": 272000,
      "visibility": "hide",
      "supported_reasoning_levels": [{"effort": "medium", "description": "Balanced"}],
      "available_in_plans": ["plus", "pro"]
    },
    {
      "slug": "gpt-5.4-mini",
      "display_name": "GPT-5.4-Mini",
      "description": "Superseded small model.",
      "context_window": 272000,
      "max_context_window": 272000,
      "visibility": "hide",
      "supported_reasoning_levels": [{"effort": "low", "description": "Fastest"}],
      "available_in_plans": ["plus", "pro"]
    },
    {
      "slug": "codex-auto-review",
      "display_name": "Codex Auto Review",
      "description": "Internal review model.",
      "context_window": 272000,
      "max_context_window": 272000,
      "visibility": "hide",
      "supported_reasoning_levels": [{"effort": "medium", "description": "Balanced"}],
      "available_in_plans": ["plus", "pro", "business"]
    }
  ]
}`

// fixtureServer serves catalogFixture and records the request it saw.
func fixtureServer(t *testing.T, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	seen := &http.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = *r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// TestListPinnedTransportBlocksRealHost is the canary for the no-live-call
// guard: it leaves BaseURL empty so List targets the real Codex backend, and
// asserts the pinned transport refuses. If this ever passes by reaching the
// network, the guard is broken and every other test in this file is suspect.
func TestListPinnedTransportBlocksRealHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("test server must not be reached when the base URL is not overridden")
	}))
	defer srv.Close()

	req := testRequest(srv)
	req.BaseURL = "" // deliberately fall through to DefaultBaseURL
	_, err := List(context.Background(), pinnedClient(t, srv.URL), req)
	if !errors.Is(err, errOffHost) {
		t.Fatalf("want errOffHost, got %v", err)
	}
}

// TestListSuccessParsesCatalog covers the happy path: request shape (path,
// required query param, auth headers) and the full 8-model mapping in vendor
// order.
func TestListSuccessParsesCatalog(t *testing.T) {
	srv, seen := fixtureServer(t, catalogFixture)

	got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if seen.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", seen.Method)
	}
	if seen.URL.Path != modelsPath {
		t.Errorf("path = %q, want %q", seen.URL.Path, modelsPath)
	}
	if v := seen.URL.Query().Get("client_version"); v != "0.0.0-test" {
		t.Errorf("client_version = %q, want %q", v, "0.0.0-test")
	}
	if v := seen.Header.Get("Authorization"); v != "Bearer test-oauth-token" {
		t.Errorf("Authorization = %q", v)
	}
	if v := seen.Header.Get("ChatGPT-Account-Id"); v != "test-account-id" {
		t.Errorf("ChatGPT-Account-Id = %q", v)
	}

	wantIDs := []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
		"gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini", "codex-auto-review",
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d models, want %d", len(got), len(wantIDs))
	}
	// Vendor order is preserved verbatim — index-by-index, not set equality.
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("model[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}

	first := got[0]
	want := Model{
		ID:            "gpt-5.6-sol",
		DisplayName:   "GPT-5.6-Sol",
		Description:   "Latest frontier agentic coding model.",
		ContextWindow: 272000,
		Visibility:    VisibilityList,
	}
	if first != want {
		t.Errorf("model[0] = %+v, want %+v", first, want)
	}
	if got[4].ContextWindow != 128000 {
		t.Errorf("codex-spark context window = %d, want 128000", got[4].ContextWindow)
	}
	if got[5].Visibility != VisibilityHide {
		t.Errorf("gpt-5.4 visibility = %q, want %q", got[5].Visibility, VisibilityHide)
	}
}

// TestSelectableFiltersHiddenAndFailsOpen pins the filter's direction: only an
// explicit "hide" is dropped, and anything unrecognized is SHOWN.
func TestSelectableFiltersHiddenAndFailsOpen(t *testing.T) {
	in := []Model{
		{ID: "listed", Visibility: VisibilityList},
		{ID: "hidden", Visibility: VisibilityHide},
		{ID: "future-value", Visibility: "experimental"}, // vendor invents a value
		{ID: "empty-value", Visibility: ""},              // field absent entirely
		{ID: "listed-2", Visibility: VisibilityList},
	}

	got := Selectable(in)
	var gotIDs []string
	for _, m := range got {
		gotIDs = append(gotIDs, m.ID)
	}
	want := []string{"listed", "future-value", "empty-value", "listed-2"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("Selectable ids = %v, want %v", gotIDs, want)
	}

	// Guard the fail-open direction explicitly: a vendor-invented visibility
	// value must never be able to empty the picker.
	onlyUnknown := Selectable([]Model{{ID: "x", Visibility: "brand-new-value"}})
	if len(onlyUnknown) != 1 {
		t.Fatalf("unrecognized visibility was filtered out: got %d models, want 1", len(onlyUnknown))
	}
}

// TestSelectableOnRealCatalog checks the filter against the observed vendor
// split: 8 models in, the 5 the vendor's own picker shows out.
func TestSelectableOnRealCatalog(t *testing.T) {
	srv, _ := fixtureServer(t, catalogFixture)
	all, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var gotIDs []string
	for _, m := range Selectable(all) {
		gotIDs = append(gotIDs, m.ID)
	}
	want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.3-codex-spark"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("selectable = %v, want %v", gotIDs, want)
	}
}

// TestListRejectsIncompleteRequestBeforeAnyRequest asserts the required inputs
// are enforced locally: the handler fails the test if it is ever reached, so a
// request issued anyway is caught rather than silently 400ing at the vendor.
func TestListRejectsIncompleteRequestBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no request may be issued for an incomplete Request")
	}))
	defer srv.Close()

	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr error
	}{
		{"missing client version", func(r *Request) { r.ClientVersion = "" }, ErrMissingClientVersion},
		{"missing token", func(r *Request) { r.Token = "" }, ErrMissingToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := testRequest(srv)
			tt.mutate(&req)
			got, err := List(context.Background(), pinnedClient(t, srv.URL), req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != nil {
				t.Errorf("models = %v, want nil", got)
			}
		})
	}
}

// TestListAccountHeaderOmittedWhenEmpty: the account id is optional, and an
// empty one is omitted rather than sent blank.
func TestListAccountHeaderOmittedWhenEmpty(t *testing.T) {
	srv, seen := fixtureServer(t, catalogFixture)
	req := testRequest(srv)
	req.AccountID = ""
	if _, err := List(context.Background(), pinnedClient(t, srv.URL), req); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, ok := seen.Header["Chatgpt-Account-Id"]; ok {
		t.Errorf("ChatGPT-Account-Id header present for empty account id: %q", seen.Header.Get("ChatGPT-Account-Id"))
	}
}

// TestListNon200TypedError asserts a non-200 is an inspectable *APIError
// carrying the status, so a caller can tell auth failure from a bad request
// from a server problem.
func TestListNon200TypedError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"bad request", http.StatusBadRequest, `{"error":{"message":"Field required"}}`},
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid token"}}`},
		{"server error", http.StatusInternalServerError, `upstream exploded`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
			if got != nil {
				t.Errorf("models = %v, want nil", got)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", apiErr.StatusCode, tt.status)
			}
			if apiErr.Body != tt.body {
				t.Errorf("body = %q, want %q", apiErr.Body, tt.body)
			}
			if !strings.Contains(apiErr.Error(), fmt.Sprint(tt.status)) {
				t.Errorf("Error() = %q, want it to mention status %d", apiErr.Error(), tt.status)
			}
		})
	}
}

// TestListMalformedJSON: a 200 with unparseable JSON is an error, not a panic
// and not a silently empty catalogue (which a picker would render as "no
// models" with no explanation).
func TestListMalformedJSON(t *testing.T) {
	for _, body := range []string{`{"models": [`, `not json at all`, ``} {
		t.Run(fmt.Sprintf("%.12q", body), func(t *testing.T) {
			srv, _ := fixtureServer(t, body)
			got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
			if err == nil {
				t.Fatalf("want decode error, got nil (models = %v)", got)
			}
			if got != nil {
				t.Errorf("models = %v, want nil on error", got)
			}
		})
	}
}

// TestListIgnoresUnknownFields: the vendor will add fields, and an addition
// must not break parsing — including a new field whose name collides with none
// of ours and one nested inside an entry.
func TestListIgnoresUnknownFields(t *testing.T) {
	const body = `{
	  "object": "list",
	  "next_page_token": null,
	  "models": [
	    {
	      "slug": "gpt-5.6-sol",
	      "display_name": "GPT-5.6-Sol",
	      "context_window": 272000,
	      "visibility": "list",
	      "brand_new_field": {"nested": [1, 2, 3]},
	      "another_new_field": "whatever"
	    }
	  ]
	}`
	srv, _ := fixtureServer(t, body)
	got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gpt-5.6-sol" || got[0].ContextWindow != 272000 {
		t.Fatalf("got %+v, want the single sol model parsed", got)
	}
}

// TestListBodyLimit: a response larger than the cap is refused with
// ErrBodyTooLarge rather than truncated into a plausible-looking short
// catalogue.
func TestListBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid JSON prefix, then padding well past the cap.
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","description":"`))
		chunk := strings.Repeat("A", 64<<10)
		for written := 0; written <= bodyLimit; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}]}`))
	}))
	defer srv.Close()

	got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	if got != nil {
		t.Errorf("models = %v, want nil", got)
	}
}

// TestListNothingInventedWhenFieldsAbsent: a sparse entry keeps its zero
// values. ContextWindow zero means UNKNOWN and must not be backfilled with a
// guessed default, and nothing resembling a price may appear from anywhere —
// subscription models have no per-token price.
func TestListNothingInventedWhenFieldsAbsent(t *testing.T) {
	const body = `{"models":[{"slug":"mystery-model"}]}`
	srv, _ := fixtureServer(t, body)
	got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d models, want 1", len(got))
	}
	want := Model{ID: "mystery-model"}
	if got[0] != want {
		t.Fatalf("got %+v, want %+v — nothing may be substituted for an absent field", got[0], want)
	}
}

// TestListContextWindowFallback: when context_window is absent but the vendor
// sent max_context_window, that vendor value is used — a fallback between two
// real values, not a guess.
func TestListContextWindowFallback(t *testing.T) {
	const body = `{"models":[{"slug":"a","max_context_window":128000},{"slug":"b","context_window":272000,"max_context_window":999}]}`
	srv, _ := fixtureServer(t, body)
	got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2", len(got))
	}
	if got[0].ContextWindow != 128000 {
		t.Errorf("fallback context window = %d, want 128000", got[0].ContextWindow)
	}
	if got[1].ContextWindow != 272000 {
		t.Errorf("context_window must win over max_context_window: got %d, want 272000", got[1].ContextWindow)
	}
}

// TestModelDeclaresNoPricingField is a structural guard for the package's
// central honesty rule: the Codex response carries no price, so Model must have
// nowhere to put one. A field a consumer could read as a price would be
// rendered as $0 — a concrete lie about cost — rather than as UNKNOWN.
func TestModelDeclaresNoPricingField(t *testing.T) {
	banned := []string{"price", "pricing", "cost", "rate", "usd", "cents"}
	rt := reflect.TypeOf(Model{})
	for i := range rt.NumField() {
		name := strings.ToLower(rt.Field(i).Name)
		for _, b := range banned {
			if strings.Contains(name, b) {
				t.Errorf("Model has pricing-shaped field %q; the vendor sends no price and none may be invented", rt.Field(i).Name)
			}
		}
	}
}

// TestListEmptyCatalogIsNotAnError: zero models is a valid answer, distinct
// from a failure.
func TestListEmptyCatalogIsNotAnError(t *testing.T) {
	srv, _ := fixtureServer(t, `{"models":[]}`)
	got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d models, want 0", len(got))
	}
}

// TestListDropsEntriesWithoutSlug: an entry with no slug names no model a
// caller could route to.
func TestListDropsEntriesWithoutSlug(t *testing.T) {
	srv, _ := fixtureServer(t, `{"models":[{"display_name":"Nameless"},{"slug":"real","visibility":"list"}]}`)
	got, err := List(context.Background(), pinnedClient(t, srv.URL), testRequest(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "real" {
		t.Fatalf("got %+v, want only the slugged model", got)
	}
}

// TestListHonorsContextCancellation: a cancelled context aborts rather than
// hanging on the vendor.
func TestListHonorsContextCancellation(t *testing.T) {
	srv, _ := fixtureServer(t, catalogFixture)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := List(ctx, pinnedClient(t, srv.URL), testRequest(srv)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
