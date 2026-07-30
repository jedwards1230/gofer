package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/search"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestNew_NoneReturnsNil covers the registration gate: New must return nil
// (not a tool that can only ever fail) when no provider is selected — the
// zero config.Search, and gofer never registers it into a session's
// registry.
func TestNew_NoneReturnsNil(t *testing.T) {
	if tl := New(config.Search{}); tl != nil {
		t.Fatalf("New(none) = %v, want nil", tl)
	}
}

// TestNew_SelectedReturnsTool covers the positive case for both backends:
// a recognized provider selection returns a non-nil tool named "web_search".
func TestNew_SelectedReturnsTool(t *testing.T) {
	for _, cfg := range []config.Search{
		{Provider: "brave", Brave: config.Brave{APIKey: "env:X"}},
		{Provider: "searxng", SearXNG: config.SearXNG{BaseURL: "http://localhost"}},
	} {
		tl := New(cfg)
		if tl == nil {
			t.Fatalf("New(%q) = nil, want a tool", cfg.Provider)
		}
		if tl.Name() != ToolName {
			t.Errorf("Name() = %q, want %q", tl.Name(), ToolName)
		}
	}
}

// TestRun_EmptyQuery covers the input-validation path: an empty/whitespace
// query never reaches the network, coming back as an IsError result (not a
// Go error — the model can react to it).
func TestRun_EmptyQuery(t *testing.T) {
	tl := New(config.Search{Provider: "brave", Brave: config.Brave{APIKey: "env:X"}})
	res, err := tl.Run(context.Background(), json.RawMessage(`{"query":"   "}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Fatalf("Run(empty query) IsError = false, want true (content: %s)", res.Content)
	}
}

// TestRun_UnresolvableCredential proves the credential is resolved AT USE
// TIME (Run), never at construction — New above never touched the env var —
// and that a resolve failure surfaces as a named IsError result rather than
// reaching the network at all.
func TestRun_UnresolvableCredential(t *testing.T) {
	const missingVar = "GOFER_WEBSEARCH_TEST_UNSET_VAR"
	tl := New(config.Search{Provider: "brave", Brave: config.Brave{APIKey: config.SecretRef("env:" + missingVar)}})
	res, err := tl.Run(context.Background(), json.RawMessage(`{"query":"golang"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Fatalf("Run IsError = false, want true (content: %s)", res.Content)
	}
	if !strings.Contains(res.Content, missingVar) {
		t.Errorf("Content = %q, want it to name %q", res.Content, missingVar)
	}
}

// TestRun_RendersBoundedResults drives Run end-to-end against a local
// httptest stub standing in for the Brave API (hermetic — no live network),
// proving: the credential resolves from config.SecretRef at Run time, the
// result text is rendered plain text (never the raw provider JSON), and
// every item the stub returned is present.
func TestRun_RendersBoundedResults(t *testing.T) {
	const apiKeyEnv = "GOFER_WEBSEARCH_TEST_API_KEY"
	t.Setenv(apiKeyEnv, "secret-token")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Subscription-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"Go Programming Language","url":"https://go.dev","description":"The Go home page."},
			{"title":"Effective Go","url":"https://go.dev/doc/effective_go","description":"Tips for writing clear Go code."}
		]}}`))
	}))
	defer srv.Close()

	tl := New(config.Search{
		Provider: "brave",
		Brave:    config.Brave{APIKey: config.SecretRef("env:" + apiKeyEnv), BaseURL: srv.URL},
	})

	res, err := tl.Run(context.Background(), json.RawMessage(`{"query":"golang"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("Run IsError = true, content: %s", res.Content)
	}
	if gotAuth != "secret-token" {
		t.Errorf("request carried API key %q, want the resolved secret", gotAuth)
	}
	for _, want := range []string{"Go Programming Language", "https://go.dev", "Effective Go"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("Content missing %q; got:\n%s", want, res.Content)
		}
	}
	// Never the raw provider JSON: the response body's field names must not
	// leak through verbatim into the rendered result.
	if strings.Contains(res.Content, `"web"`) || strings.Contains(res.Content, `"results"`) {
		t.Errorf("Content looks like raw provider JSON, want rendered text:\n%s", res.Content)
	}
}

// TestRenderResults_Bounded covers renderResults directly against a
// [search.Results] value that already carries the SDK's own bounding
// (Truncated, a capped Items slice) — this package's rendering must reflect
// that bounding in the text, not just happen to pass it through unbounded
// input.
func TestRenderResults_Bounded(t *testing.T) {
	got := renderResults(&search.Results{
		Query:     "golang",
		Provider:  "brave",
		Truncated: true,
		Items: []search.Result{
			{Rank: 1, Title: "A", URL: "https://a.example", Snippet: "snippet a"},
		},
	})
	for _, want := range []string{"golang", "brave", "[truncated]", "A", "https://a.example", "snippet a"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderResults output missing %q; got:\n%s", want, got)
		}
	}
}

// TestRenderResults_NoItems covers the empty-results case: no items yields a
// readable "no results" line, never an empty string or a panic on a nil
// Items slice.
func TestRenderResults_NoItems(t *testing.T) {
	got := renderResults(&search.Results{Query: "nothing found for this"})
	if !strings.Contains(got, "nothing found for this") {
		t.Errorf("renderResults(no items) = %q, want it to name the query", got)
	}
}

// TestSearchBuild_UnknownProviderNamesTheLikelyCause proves the SDK's own
// search.Build error already names the likely cause (a missing blank
// import) and lists what IS registered — and, since this package blank-
// imports both search/brave and search/searxng, that list includes both.
// gofer therefore wraps nothing further at this seam (see the package doc);
// this test is the proof that wrapping is unnecessary, not merely an
// assertion that it is absent.
func TestSearchBuild_UnknownProviderNamesTheLikelyCause(t *testing.T) {
	_, err := search.Build("does-not-exist", search.Config{})
	if err == nil {
		t.Fatal("search.Build(unknown) returned nil error")
	}
	msg := err.Error()
	for _, want := range []string{"does-not-exist", "blank import", "brave", "searxng"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}
