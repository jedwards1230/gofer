package config

import (
	"errors"
	"fmt"
	"time"
)

// SearchProvider is the resolved form of [Search.Provider]'s free-text
// config value.
type SearchProvider string

const (
	// SearchProviderNone registers no search tool at all; the fail-safe
	// default. See [Search.Selected].
	SearchProviderNone    SearchProvider = "none"
	SearchProviderBrave   SearchProvider = "brave"
	SearchProviderSearXNG SearchProvider = "searxng"
)

const (
	// DefaultBraveBaseURL is [Brave.BaseURL]'s default. A const rather than
	// a literal buried in a client, so an operator can point at a proxy or
	// a pinned API version via config instead of a code change.
	DefaultBraveBaseURL = "https://api.search.brave.com/res/v1/web/search"
	// DefaultSearchTimeout is [Search.TimeoutMS]'s default: 10s. A search
	// API answers in under a second when healthy, so past ten seconds the
	// turn is better served by a clear error than a stall.
	DefaultSearchTimeout = 10 * time.Second
	// DefaultSearchMaxResults is [Search.MaxResults]'s default: 5. Each
	// result costs roughly 60-100 tokens of title+URL+snippet, so five is
	// a useful answer at roughly 500 tokens.
	DefaultSearchMaxResults = 5
)

// Search configures gofer's web-search tool: which provider, if any, backs
// it, and the shared timeout/result-count knobs both providers honor. The
// zero value is fully valid and resolves to [SearchProviderNone] — no
// search tool is registered at all; see [Search.Selected].
type Search struct {
	// Provider selects the backend; see [SearchProvider]. Read through
	// [Search.Selected], which fails safe to none.
	Provider string `json:"provider,omitempty"`
	// Brave configures the Brave Search API provider; only consulted when
	// Provider selects it.
	Brave Brave `json:"brave,omitempty"`
	// SearXNG configures a self-hosted SearXNG instance; only consulted
	// when Provider selects it.
	SearXNG SearXNG `json:"searxng,omitempty"`
	// TimeoutMS bounds one search call; see [Search.Timeout].
	TimeoutMS *int `json:"timeout_ms,omitempty"`
	// MaxResults caps how many results one search call returns; see
	// [Search.ResultLimit].
	MaxResults *int `json:"max_results,omitempty"`
}

// Brave configures the Brave Search API provider.
type Brave struct {
	// APIKey is a [SecretRef], never an inline token — see
	// [Config.validate].
	APIKey SecretRef `json:"api_key,omitempty"`
	// BaseURL overrides [DefaultBraveBaseURL]; see [Brave.Endpoint].
	BaseURL string `json:"base_url,omitempty"`
}

// SearXNG configures a self-hosted SearXNG instance. There is deliberately
// NO default BaseURL: no public SearXNG instance is fit to default to, and
// shipping one would silently point every unconfigured install at somebody
// else's server.
type SearXNG struct {
	BaseURL string    `json:"base_url,omitempty"`
	APIKey  SecretRef `json:"api_key,omitempty"`
}

// Selected resolves [Search.Provider] to a [SearchProvider]. Only the exact
// spellings "brave"/"searxng" opt in; unset and anything else — including a
// provider selected but left unconfigured — resolve to
// [SearchProviderNone].
//
// This fails safe toward NO search tool for two reasons at once, not one:
// first, a provider selected but unconfigured (no API key, no base URL)
// would register a tool that always errors — worse for both context cost
// and the model's own planning than the tool simply not existing at all.
// Second, web search means outbound traffic to a third party and, for
// Brave, a paid quota — neither is something an upgrade should switch on
// silently. (A genuinely misconfigured selection — "brave" with no
// api_key — is instead a hard [Config.validate] error at load time; see
// [Search.validate]. Selected only ever resolves an already-valid config.)
func (s Search) Selected() SearchProvider {
	switch SearchProvider(s.Provider) {
	case SearchProviderBrave, SearchProviderSearXNG:
		return SearchProvider(s.Provider)
	default:
		return SearchProviderNone
	}
}

// Timeout resolves [Search.TimeoutMS]'s effective value:
// [DefaultSearchTimeout] when unset or non-positive.
func (s Search) Timeout() time.Duration {
	if s.TimeoutMS == nil || *s.TimeoutMS <= 0 {
		return DefaultSearchTimeout
	}
	return time.Duration(*s.TimeoutMS) * time.Millisecond
}

// ResultLimit resolves [Search.MaxResults]'s effective value:
// [DefaultSearchMaxResults] when unset or non-positive.
func (s Search) ResultLimit() int {
	if s.MaxResults == nil || *s.MaxResults <= 0 {
		return DefaultSearchMaxResults
	}
	return *s.MaxResults
}

// Endpoint resolves [Brave.BaseURL]'s effective value: [DefaultBraveBaseURL]
// when unset.
func (b Brave) Endpoint() string {
	if b.BaseURL == "" {
		return DefaultBraveBaseURL
	}
	return b.BaseURL
}

// validate rejects a selected provider left unusably unconfigured: searxng
// with no base_url (it can never work, and the fix is one config line), or
// brave with no api_key (same reasoning). Unlike [Search.Selected]'s
// fail-safe-to-none polarity for an UNRECOGNIZED provider string, a
// RECOGNIZED-but-broken selection is a load error — the operator asked for
// a specific provider and typo'd its one required setting, which they need
// to hear about immediately rather than silently getting no search tool.
func (s Search) validate() error {
	switch SearchProvider(s.Provider) {
	case SearchProviderSearXNG:
		if s.SearXNG.BaseURL == "" {
			return errors.New("search: searxng selected but searxng.base_url is not configured")
		}
	case SearchProviderBrave:
		if s.Brave.APIKey == "" {
			return errors.New("search: brave selected but brave.api_key is not configured")
		}
	}
	if err := s.Brave.APIKey.validate(); err != nil {
		return fmt.Errorf("search: brave.api_key: %w", err)
	}
	if err := s.SearXNG.APIKey.validate(); err != nil {
		return fmt.Errorf("search: searxng.api_key: %w", err)
	}
	return nil
}
