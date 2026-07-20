package modelcatalog

// discovery.go holds the live model-discovery layer that sits above the static
// floor in modelcatalog.go: the options that enable it, the credential
// plumbing that feeds it, and the fallback rule. The HTTP call, JSON shape, and
// the visibility rule belong to the SDK's provider.ModelLister (implemented by
// provider/openai); this file owns everything on gofer's side of that boundary
// — which credential is used, how long the call may take, what a failure means,
// and how a listing entry becomes a picker row. See the package doc for why
// there are two sources and why the floor can never be skipped.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jedwards1230/agent-sdk-go/auth"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/openai"

	"github.com/jedwards1230/gofer/internal/modelmeta"
)

// CodexClientVersion is the value sent for the listing's client_version query
// parameter.
//
// What the parameter is: an identifier for the calling client. It is REQUIRED —
// omitting it makes the endpoint answer HTTP 400 ("Field required"), which
// reads exactly like a nonexistent endpoint and is the likely origin of the
// belief that the Codex backend has no models listing at all.
//
// What is NOT known: which values the backend actually accepts. Testing found
// both a real vendor client release and a plainly synthetic "1.0.0" answered
// HTTP 200, so the parameter does not appear to be validated against a release
// list — but that is an observation from two samples, not a documented
// contract, and the backend is free to start validating it. This is pinned to a
// real vendor client release on the theory that a genuine version is the least
// likely to be rejected or to receive a narrowed listing. Revisit it if
// discovery starts failing or starts omitting models known to exist.
//
// The SDK carries its own default for this parameter, and today the two values
// agree. gofer still passes this one explicitly ([openai.WithCodexClientVersion]):
// the agreement is a coincidence of the current release, not a contract, and the
// choice of what version gofer claims to be is gofer's to make and to revisit
// against gofer's own evidence — not something to inherit silently on the next
// SDK bump.
const CodexClientVersion = "0.144.3"

// DefaultDiscoveryTimeout bounds live discovery, including the credential
// resolution that precedes it. It exists so a slow or black-holed vendor host
// degrades to the static floor promptly instead of stalling a picker.
//
// It is a package default rather than a literal at the call site so a caller
// can override it ([WithTimeout]); promoting it to config.json is a reasonable
// follow-up if a user ever needs to tune it.
const DefaultDiscoveryTimeout = 3 * time.Second

// Option configures a catalog lookup.
type Option func(*options)

type options struct {
	discover bool
	httpc    *http.Client
	baseURL  string
	timeout  time.Duration
}

func newOptions(opts []Option) options {
	o := options{timeout: DefaultDiscoveryTimeout}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithDiscovery enables live discovery against the Codex listing.
//
// Production callers pass WithDiscovery(nil, ""): a nil client means
// http.DefaultClient and an empty base URL means the SDK's own OAuth route (the
// Codex backend), since the adapter derives the base URL from the credential
// kind when none is given. Tests pass a pinned transport and an httptest URL —
// the override redirects the host only, leaving the credential-kind routing and
// therefore the wire contract intact, which is what makes a test against a local
// server exercise the real Codex path.
//
// Discovery is OPT-IN, and deliberately so. A lookup with no options performs
// no network call of any kind, which means no test — present or future — can
// reach a real vendor host by forgetting to stub something. Making live the
// silent default would invert that: the safe path would require remembering an
// option, and the unsafe path would be the one you get by saying nothing. The
// cost of this choice is that a caller who wants live data must ask for it;
// that is a visible, reviewable omission, whereas an accidental billed request
// is neither.
func WithDiscovery(httpc *http.Client, baseURL string) Option {
	return func(o *options) {
		o.discover = true
		o.httpc = httpc
		o.baseURL = baseURL
	}
}

// WithTimeout overrides [DefaultDiscoveryTimeout] for one lookup. A
// non-positive d restores the default rather than disabling the bound: an
// unbounded vendor call on a path a user is waiting behind is never the
// intended behavior, so it cannot be requested by accident.
func WithTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// discoverCodex runs live discovery for an OpenAI OAuth credential.
//
// It returns an error for EVERY failure mode — no credential, an unusable
// credential, a 400/401, a malformed body, a timeout, or an empty listing —
// because every one of them means the same thing to the caller: fall back to
// the static floor. Callers do not branch on which failure occurred.
//
// An empty listing is treated as a failure on purpose, even though the SDK
// correctly reports it as a success with zero models. A 200 carrying no models
// would otherwise produce an empty picker, which is the exact outcome the floor
// exists to prevent; a vendor returning nothing is indistinguishable from one
// that is broken, and a stale list beats no list.
//
// The call itself is the SDK's [provider.ModelLister], which owns the endpoint,
// the required client_version parameter, the response shape, and the
// visibility-to-Hidden normalization. gofer supplies the three things that are
// its own policy and cannot be delegated: the credential (already resolved and
// refreshed here, so the adapter re-resolves nothing), the deadline, and the
// rule that says a failure means the floor.
func discoverCodex(ctx context.Context, root string, o options) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	token, accountID, err := codexCredential(ctx, root, o.httpc)
	if err != nil {
		return nil, err
	}

	// The credential is handed over as a static source rather than the auth
	// store itself, even though auth.Store satisfies the interface. Two reasons,
	// both load-bearing: the refresh above has already happened under this
	// function's deadline (see codexCredential), so a second resolution would be
	// redundant work on a latency-sensitive path; and pinning Kind to OAuth here
	// keeps the adapter on the Codex wire contract, which is the only one this
	// function is about. Passing the store would let a non-OAuth entry silently
	// re-route the call to the public API's differently-shaped listing instead
	// of failing the kind check codexCredential performs.
	lister := openai.New("", provider.StaticCredentialSource{Cred: provider.Credential{
		Kind:    provider.CredOAuth,
		Token:   token,
		Account: accountID,
	}},
		openai.WithHTTPClient(o.httpc),
		openai.WithBaseURL(o.baseURL),
		openai.WithCodexClientVersion(CodexClientVersion),
	)

	found, err := lister.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover codex models: %w", err)
	}

	out := make([]Model, 0, len(found))
	for _, m := range found {
		// Hidden is the vendor's "do not offer this in a picker" marker, and the
		// SDK sets it ONLY on an exact "hide" — every other visibility value,
		// including one neither gofer nor the SDK has ever seen, arrives as
		// false and is kept. That fail-OPEN direction is the whole point: if an
		// unrecognized marker meant hidden, the day the vendor renames its
		// visible value the picker would silently go empty. Showing a model the
		// vendor tucks away is far cheaper, and such a model is still routable.
		if m.Hidden {
			continue
		}
		out = append(out, Model{
			ID:       m.ID,
			Provider: providerOpenAI,
			// The live display name wins over gofer's own table: it is the
			// vendor's current name, while modelmeta is a compiled-in guess
			// that can only be as fresh as this binary. The table still
			// backstops an entry the listing names with nothing.
			Label: labelFor(m.ID, m.DisplayName),
			// Carried through verbatim, zero included — zero means UNKNOWN in
			// both this type and the vendor's, and inventing a number here
			// would be the same class of error as inventing a price.
			ContextWindow: m.ContextWindow,
		})
	}
	// One check covers both empty cases, which mean the same thing downstream: a
	// vendor that listed nothing at all, and one whose entire listing was marked
	// hidden. Either way there is no picker to build, so the floor takes over.
	//
	// Pricing is absent from Model by construction, so nothing above could have
	// carried one across even if the SDK had offered it — which it does not: a
	// ModelLister record's Pricing is unconditionally zero, and zero here would
	// mean UNKNOWN, never free.
	if len(out) == 0 {
		return nil, errors.New("discover codex models: listing offered no selectable models")
	}
	return out, nil
}

// codexCredential resolves the bearer token and ChatGPT account id for live
// discovery.
//
// This is the ONE path in this package that calls auth.Store.Credential rather
// than the non-refreshing auth.Store.Get, and the choice is deliberate. There
// is a real tension: Credential can perform a network token refresh, which is
// exactly what the kind-only paths avoid. It is the right call *here* and
// nowhere else, for three reasons:
//
//  1. This path is already making a network request, so a refresh adds a
//     round trip to an operation that has one — not to one that has none.
//  2. The listing needs a VALID access token. Get returns the persisted token
//     including an expired one, which can only produce a 401 and a permanent
//     silent downgrade to the static floor for any user whose token has
//     lapsed — which is the normal state of an OAuth credential between
//     refreshes, not an edge case.
//  3. The refresh is covered by the same deadline as the request
//     ([DefaultDiscoveryTimeout]), so the worst case is still a bounded wait
//     ending in the floor.
//
// The startup path is unaffected: [DefaultModel] never reaches this function,
// so no `gofer run` can block on a token refresh. That is what keeps "an
// expired token 401s into the floor" acceptable while "startup hangs" stays
// impossible.
func codexCredential(ctx context.Context, root string, httpc *http.Client) (token, accountID string, err error) {
	opts := []auth.Option{auth.WithRoot(root)}
	if httpc != nil {
		// The SAME client that serves the listing also serves the token
		// refresh. That is not a convenience: the refresh posts to a real
		// vendor host (the SDK's token URL), so if only the listing client were
		// injectable, a test with an expired credential would issue a live
		// request to the vendor's auth endpoint through the store's default
		// client — pinning one transport but not the other. One injection point
		// makes both calls pinnable together and neither pinnable alone.
		opts = append(opts, auth.WithHTTPClient(httpc))
	}
	store, err := auth.New(opts...)
	if err != nil {
		return "", "", fmt.Errorf("open auth store: %w", err)
	}
	cred, err := store.Credential(ctx, providerOpenAI)
	if err != nil {
		return "", "", fmt.Errorf("resolve openai credential: %w", err)
	}
	if cred.Kind != auth.KindOAuth || cred.Token == "" {
		return "", "", errors.New("openai credential is not a usable oauth token")
	}
	return cred.Token, cred.Account, nil
}

// labelFor prefers the live display name and falls back to gofer's table, which
// itself falls back to the raw id — so the result is never blank.
func labelFor(id, liveName string) string {
	if liveName != "" {
		return liveName
	}
	return modelmeta.DisplayName(id)
}
