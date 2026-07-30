// Package websearch is gofer's `web_search` [tool.Tool]: the model-facing
// projection of the SDK's provider-agnostic `search` package (M7 workstream
// 4). The SDK ships the [search.Provider] interface, the Brave and SearXNG
// backends, and a name-keyed registry, but no tool — an operator could
// configure a provider and credentials would resolve, yet the agent would
// still have no way to search. This package is that missing tool.
//
// # Registration is conditional
//
// [New] returns nil when [config.Search.Selected] resolves to
// [config.SearchProviderNone] — the caller (internal/supervisor's
// sessionGuard) must check for nil and skip registration entirely rather
// than register a tool that can only ever error. This mirrors
// [config.Search.Selected]'s own fail-safe-to-none polarity: web search is
// outbound third-party traffic (and, for Brave, a paid quota), so it must
// never appear in a session's tool surface an operator didn't ask for.
//
// # Credentials resolve at use time
//
// [Tool.Run] resolves the configured [config.SecretRef] and builds the
// [search.Provider] fresh on EVERY call — never once at construction or at
// config load. This is deliberate, not an oversight: a credential a session
// never actually calls (an unselected or misconfigured provider a session
// happens not to use) must not break every other config section just
// because its env var is unset on this host, and [config.SecretRef.Resolve]
// is documented to run at use time for exactly that reason. Building a
// [search.Provider] is a cheap, I/O-free struct construction, so paying that
// cost per call is not a real overhead.
//
// # Results are bounded, never raw provider JSON
//
// [Tool.Run]'s Content is rendered from [search.Results] — already bounded
// by the SDK ([search.ClampMaxResults], [search.TruncateSnippet]) — as
// plain, numbered lines, never a JSON dump of the provider's raw response.
package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/search"
	// Blank-imported for their init() self-registration side effect: without
	// these, search.Build("brave", …) / search.Build("searxng", …) fail with
	// "no provider registered" against a config that is otherwise perfectly
	// valid — a genuinely confusing hour for whoever hits it. gofer imports
	// BOTH backends unconditionally; the operator selects at runtime via
	// config.Search.Provider, and both packages are tiny.
	_ "github.com/jedwards1230/agent-sdk-go/search/brave"
	_ "github.com/jedwards1230/agent-sdk-go/search/searxng"
	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/config"
)

// ToolName is the name the model calls the web-search tool by.
const ToolName = "web_search"

// Tool is the `web_search` [tool.Tool]. It carries only cfg — see the
// package doc for why the provider itself is built fresh on every Run.
type Tool struct {
	cfg config.Search
}

// Tool is a tool the SDK's registry accepts.
var _ tool.Tool = (*Tool)(nil)

// New returns the web_search tool for cfg, or nil when cfg.Selected()
// resolves to [config.SearchProviderNone] — see the package doc. Callers
// MUST check for a nil return and skip registration; New never returns a
// tool that can only ever fail.
func New(cfg config.Search) tool.Tool {
	if cfg.Selected() == config.SearchProviderNone {
		return nil
	}
	return &Tool{cfg: cfg}
}

// Name returns "web_search".
func (*Tool) Name() string { return ToolName }

// Description returns the model-facing description.
func (*Tool) Description() string {
	return "Search the web and return a bounded, ranked set of results (title, URL, " +
		"short snippet) from the configured provider. Use it for information outside " +
		"the codebase and outside your training data — current events, documentation " +
		"for a library version, an error message you don't recognize. Results are " +
		"summaries, not page contents; fetch a URL yourself if you need the full text."
}

// Spec returns the JSON Schema for the tool's input: a required "query" and
// an optional "max_results" override, clamped server-side regardless of what
// is requested (see [search.ClampMaxResults]).
func (*Tool) Spec() tool.Schema {
	return tool.ObjectSchema([]string{"query"}, map[string]tool.Property{
		"query": {
			Type:        "string",
			Description: "The search query.",
		},
		"max_results": {
			Type:        "integer",
			Description: "Maximum results to return; defaults to the configured limit and is always capped server-side.",
		},
	})
}

// input is the decoded shape of Run's argument, matching Spec exactly.
type input struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// Run resolves t.cfg's credential, builds the configured [search.Provider]
// fresh (see the package doc), issues the query, and renders the bounded
// [search.Results] as plain text. Every failure a model could not have
// caused by malformed input — an unresolvable credential, an unlinked
// provider, a request/HTTP/decode failure — comes back as an IsError
// [tool.Result], per [tool.Tool.Run]'s contract, not a Go error: none of
// them are ctx cancellation or bad input JSON, the only two cases that
// contract reserves for a Go error.
func (t *Tool) Run(ctx context.Context, in json.RawMessage) (tool.Result, error) {
	var params input
	if len(in) > 0 {
		if err := json.Unmarshal(in, &params); err != nil {
			return tool.Result{}, fmt.Errorf("%s: decode input: %w", ToolName, err)
		}
	}
	if strings.TrimSpace(params.Query) == "" {
		return tool.Result{IsError: true, Content: ToolName + ": query must not be empty"}, nil
	}

	provider, err := t.buildProvider()
	if err != nil {
		return tool.Result{IsError: true, Content: fmt.Sprintf("%s: %v", ToolName, err)}, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout())
	defer cancel()
	results, err := provider.Search(callCtx, params.Query, search.Options{MaxResults: params.MaxResults})
	if err != nil {
		return tool.Result{IsError: true, Content: fmt.Sprintf("%s: %v", ToolName, err)}, nil
	}
	return tool.Result{Content: renderResults(results)}, nil
}

// buildProvider resolves t.cfg's credential and endpoint for the SELECTED
// provider and builds it via [search.Build] — called fresh from every Run,
// never cached (see the package doc's "credentials resolve at use time").
// config.Search.Selected() already fails safe to none for an unrecognized
// provider string, and [config.Search.validate] already rejects a selected-
// but-unconfigured provider at config load — so the only way Selected()
// resolves to Brave or SearXNG here is a valid, already-validated config;
// the default case is unreachable outside a test double.
func (t *Tool) buildProvider() (search.Provider, error) {
	switch t.cfg.Selected() {
	case config.SearchProviderBrave:
		apiKey, err := t.cfg.Brave.APIKey.Resolve()
		if err != nil {
			return nil, fmt.Errorf("resolve brave.api_key: %w", err)
		}
		return search.Build("brave", search.Config{
			APIKey:     apiKey,
			BaseURL:    t.cfg.Brave.Endpoint(),
			MaxResults: t.cfg.ResultLimit(),
		})
	case config.SearchProviderSearXNG:
		apiKey, err := t.cfg.SearXNG.APIKey.Resolve()
		if err != nil {
			return nil, fmt.Errorf("resolve searxng.api_key: %w", err)
		}
		return search.Build("searxng", search.Config{
			APIKey:     apiKey,
			BaseURL:    t.cfg.SearXNG.BaseURL,
			MaxResults: t.cfg.ResultLimit(),
		})
	default:
		return nil, fmt.Errorf("no search provider selected")
	}
}

// renderResults renders results as plain, numbered lines — never the raw
// provider JSON. Every field it reads is already bounded by the SDK before
// it reaches here (see the package doc).
func renderResults(results *search.Results) string {
	if results == nil {
		return "no results"
	}
	if len(results.Items) == 0 {
		return fmt.Sprintf("no results for %q", results.Query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d results for %q (provider: %s)", len(results.Items), results.Query, results.Provider)
	if results.Truncated {
		b.WriteString(" [truncated]")
	}
	for _, r := range results.Items {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n   %s", r.Rank, r.Title, r.URL, r.Snippet)
	}
	return b.String()
}
