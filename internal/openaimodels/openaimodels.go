// Package openaimodels is a client for the Codex backend's model listing —
// the catalogue an OpenAI OAuth (ChatGPT-subscription) credential can actually
// reach.
//
// It exists because OpenAI serves two different model families depending on the
// credential kind: an API key targets the public API, while an OAuth credential
// targets the Codex backend, whose family the SDK registry does not carry. The
// Codex backend does enumerate its own models, so a picker under OAuth need not
// rely solely on a compiled-in table.
//
// # Purity
//
// The package is a leaf: stdlib only, no gofer or SDK imports, no global state,
// and nothing read from the environment or an auth store. Every input — HTTP
// client, base URL, token, account id, client version — is injected by the
// caller, which is what makes the whole surface testable against an httptest
// server and impossible to point at a real vendor host by accident.
//
// # No pricing, ever
//
// The vendor response carries NO price field, because a subscription has no
// per-token price. [Model] therefore has no pricing field at all: a consumer
// must render price as UNKNOWN, never as $0. Nothing here invents, defaults, or
// backfills a price, and [TestModelDeclaresNoPricingField] guards that.
//
// # Not an admission gate
//
// This is display metadata. A listing that omits a model is not a statement
// that the model is unusable — a caller must still run any id the user names.
package openaimodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DefaultBaseURL is the Codex backend root an OAuth credential routes to. It
// matches the SDK's OAuth route (agent-sdk-go provider/openai's route()).
const DefaultBaseURL = "https://chatgpt.com/backend-api/codex"

// modelsPath is the listing endpoint, relative to the base URL.
const modelsPath = "/models"

// bodyLimit caps how much of a listing response is read, so a misrouted or
// hostile endpoint cannot stream unbounded data into memory. It matches the cap
// the SDK uses for the analogous listing path.
const bodyLimit = 8 << 20

// errBodyLimit caps how much of a non-200 response body is carried back in an
// [APIError], so a huge error page cannot be pulled into memory or a log line.
const errBodyLimit = 64 << 10

// Visibility values observed from the vendor. The set is NOT closed — see
// [Selectable] for how an unrecognized value is treated.
const (
	// VisibilityList marks a model the vendor's own picker shows.
	VisibilityList = "list"
	// VisibilityHide marks a model the vendor's own picker omits (internal or
	// retired ids that are still routable).
	VisibilityHide = "hide"
)

// Model is one entry of the Codex model catalogue.
//
// There is deliberately no pricing field: see the package doc. A zero
// ContextWindow means UNKNOWN, not "no context" — the vendor omitted it and
// this package does not guess.
type Model struct {
	// ID is the model id to route with, from the vendor's "slug".
	ID string
	// DisplayName is the vendor's human label, e.g. "GPT-5.6-Sol". May be empty.
	DisplayName string
	// Description is the vendor's one-line blurb. May be empty.
	Description string
	// ContextWindow is the usable context in tokens; zero means UNKNOWN.
	ContextWindow int
	// Visibility is the vendor's visibility value, carried through verbatim —
	// including a value this package does not recognize.
	Visibility string
}

// Request carries the inputs of a listing call.
//
// It is a struct rather than a run of positional string parameters because
// BaseURL, Token, AccountID and ClientVersion are all strings: positionally,
// swapping any two compiles cleanly and fails only against the live vendor.
type Request struct {
	// BaseURL is the backend root. Empty means [DefaultBaseURL].
	BaseURL string
	// Token is the OAuth access token, sent as a bearer token. Required.
	Token string
	// AccountID is the ChatGPT account id. Optional: when empty the header is
	// omitted rather than sent blank.
	AccountID string
	// ClientVersion populates the REQUIRED client_version query parameter.
	// Required — see [ErrMissingClientVersion].
	ClientVersion string
}

// Errors reported before any request is issued.
var (
	// ErrMissingClientVersion reports an empty [Request.ClientVersion].
	//
	// client_version is a REQUIRED query parameter: omitting it makes the
	// endpoint answer HTTP 400 ("Field required") rather than serve a
	// catalogue, which reads exactly like a nonexistent endpoint. Rejecting it
	// here means a caller cannot spend a round trip discovering that.
	ErrMissingClientVersion = errors.New("openaimodels: client_version is required")

	// ErrMissingToken reports an empty [Request.Token]. A request with no
	// Authorization header can only be answered with 401.
	ErrMissingToken = errors.New("openaimodels: token is required")

	// ErrBodyTooLarge reports a listing response exceeding the read cap. It is
	// surfaced rather than silently truncated: a truncated catalogue is
	// indistinguishable from a short one, and would quietly hide models.
	ErrBodyTooLarge = errors.New("openaimodels: response body exceeds limit")
)

// APIError is a non-200 response from the listing endpoint. It carries the
// status so a caller can tell an auth failure (401/403) from a malformed
// request (400) from a server problem (5xx), and a truncated body for
// diagnosis.
type APIError struct {
	// StatusCode is the HTTP status of the response.
	StatusCode int
	// Body is the response body, trimmed and truncated to errBodyLimit.
	Body string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("openaimodels: http %d", e.StatusCode)
	}
	return fmt.Sprintf("openaimodels: http %d: %s", e.StatusCode, e.Body)
}

// List fetches the model catalogue from the Codex backend.
//
// It preserves the vendor's ordering and does not sort: the vendor's order is
// the one its own picker presents, and reordering would silently disagree with
// it. Unknown JSON fields are ignored, so a vendor addition is not an error.
// Entries with no slug are dropped — they name no model a caller could route to.
//
// A catalogue of zero models is an empty slice and a nil error, which is
// distinct from a failure.
//
// httpc may be nil, in which case [http.DefaultClient] is used; inject a client
// with a timeout (and, in tests, a pinned transport) for anything real.
func List(ctx context.Context, httpc *http.Client, req Request) ([]Model, error) {
	if req.ClientVersion == "" {
		return nil, ErrMissingClientVersion
	}
	if req.Token == "" {
		return nil, ErrMissingToken
	}
	if httpc == nil {
		httpc = http.DefaultClient
	}

	base := req.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	endpoint := strings.TrimSuffix(base, "/") + modelsPath
	q := url.Values{"client_version": []string{req.ClientVersion}}
	endpoint += "?" + q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("openaimodels: new request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Token)
	if req.AccountID != "" {
		httpReq.Header.Set("ChatGPT-Account-Id", req.AccountID)
	}

	resp, err := httpc.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("openaimodels: list models: %w", ctxErr)
		}
		return nil, fmt.Errorf("openaimodels: list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		return nil, &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}

	// Read one byte past the cap so an oversized body is detectable rather than
	// silently truncated into a short catalogue.
	data, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("openaimodels: list models: %w", ctxErr)
		}
		return nil, fmt.Errorf("openaimodels: list models: read body: %w", err)
	}
	if len(data) > bodyLimit {
		return nil, ErrBodyTooLarge
	}

	var res struct {
		Models []struct {
			Slug             string `json:"slug"`
			DisplayName      string `json:"display_name"`
			Description      string `json:"description"`
			ContextWindow    int    `json:"context_window"`
			MaxContextWindow int    `json:"max_context_window"`
			Visibility       string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("openaimodels: list models: decode response: %w", err)
	}

	out := make([]Model, 0, len(res.Models))
	for _, m := range res.Models {
		if m.Slug == "" {
			continue
		}
		// max_context_window is the vendor's own value for the same quantity
		// and is used only when context_window is absent. That is a fallback
		// between two vendor-supplied numbers, not a guess: if both are absent
		// the result stays zero, meaning UNKNOWN.
		ctxWindow := m.ContextWindow
		if ctxWindow == 0 {
			ctxWindow = m.MaxContextWindow
		}
		out = append(out, Model{
			ID:            m.Slug,
			DisplayName:   m.DisplayName,
			Description:   m.Description,
			ContextWindow: ctxWindow,
			Visibility:    m.Visibility,
		})
	}
	return out, nil
}

// Selectable returns the models a user should be offered, preserving order.
//
// Only an explicit [VisibilityHide] is excluded. Every other value — including
// one this package does not recognize, and the empty string — is kept.
//
// That fail-OPEN direction is deliberate. The visibility vocabulary belongs to
// the vendor and can grow at any time; if a new value were treated as hidden,
// the day the vendor renames "list" the picker would silently go empty and the
// user would have no models at all. Showing a model that the vendor's own
// picker happens to hide is a far cheaper mistake than showing none, especially
// since a hidden model is still routable.
func Selectable(models []Model) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if m.Visibility == VisibilityHide {
			continue
		}
		out = append(out, m)
	}
	return out
}
