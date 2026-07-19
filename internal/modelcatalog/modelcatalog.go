// Package modelcatalog answers "which models can THIS credential actually
// reach, and which one should gofer default to?".
//
// The SDK answers both questions by provider id alone (runner.DefaultModel,
// provider.Models), which is wrong for OpenAI: the provider routes by
// credential KIND (agent-sdk-go provider/openai's route()). An API key targets
// the public API and serves the gpt-5 family the SDK registry carries; an
// OAuth (ChatGPT-subscription) credential targets the Codex backend, which
// serves a different family and rejects gpt-5 outright with HTTP 400. A fresh
// install + OAuth login + first message was therefore an immediate 400 (issue
// #157). Anthropic serves one family on both kinds, so it is unaffected and
// keeps delegating to the SDK.
//
// The package is a leaf over the SDK (no gofer imports beyond
// internal/modelmeta, itself stdlib-only), so both cmd/gofer (daemon/run) and
// internal/tui (the /model picker) can use it with no import cycle. It splits
// into a pure core — [DefaultModelForKind], [CatalogForKind], no IO — and a
// thin root-reading shell — [CredentialKind], [DefaultModel], [Catalog] — so a
// caller that already knows the credential kind (the TUI, from CommandEnv.Auth)
// need not touch the disk again.
//
// # No vendor IO, ever
//
// Resolution happens on every start, so it must not make a vendor call. Kind is
// read with auth.Store.Get, which is a plain read of auth.json. It is
// deliberately NOT auth.Store.Credential: Credential transparently refreshes an
// expired OAuth token, i.e. it can perform a live token-endpoint request
// (agent-sdk-go auth/store.go, Credential -> refreshEntry). Get returns the
// same persisted Kind with no network, no refresh, and no write lock.
//
// # Two sources: live discovery over a static floor
//
// The Codex backend DOES enumerate its own models — this is verified, not
// assumed. Its listing takes a REQUIRED client_version query parameter, and
// omitting that parameter is itself a 400, which is the likely origin of the
// widespread "the Codex backend has no models endpoint" belief. Discovery is
// therefore the primary source, reached through the [DiscoverFunc] seam.
//
// [codexModels] is the offline FLOOR beneath it, used when discovery is
// unavailable, errors, times out, returns nothing, or the credential is not
// OAuth. The floor exists because of one hard requirement: a failed discovery
// must degrade to a stale-but-usable list, NEVER to an empty picker. A user on
// a flaky network still gets a working model list.
//
// Neither source is an admission gate. gofer must keep running any model id the
// user names, listed or not: since SDK v0.12.0 provider.Resolve admits an
// unregistered id by inferring its backend from the id's shape, and the
// picker's free-text entry line depends on that. Nothing in this package
// validates, filters, or rejects a caller-supplied id — see
// TestCatalogIsNotAnAdmissionGate.
//
// # No pricing, ever
//
// The live listing carries no pricing field, because a subscription model has
// no per-token price. [Model] therefore has no pricing member and this package
// never synthesizes one. A renderer must show pricing for these ids as UNKNOWN;
// showing $0 would state "free" as fact, which is a fabricated price. The same
// rule the SDK applies to provider.ModelInfo.Unregistered applies here.
package modelcatalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/jedwards1230/agent-sdk-go/auth"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/modelmeta"
)

// Kind is a credential's kind. Its values mirror the SDK's auth.CredKind
// ("oauth" | "api_key") — see kindsMirrorSDK in the tests, which locks the two
// together — so the pure [DefaultModelForKind] / [CatalogForKind] entry points
// take a plain gofer type and a caller that already has a kind in hand (the
// TUI's tui.AuthKind, itself the same mirror) converts with a string cast
// rather than importing the SDK's auth package.
type Kind string

const (
	// KindOAuth is a subscription OAuth access token (mirrors auth.KindOAuth).
	// For OpenAI this is the ChatGPT-subscription credential that routes to the
	// Codex backend.
	KindOAuth Kind = "oauth"
	// KindAPIKey is a long-lived API key (mirrors auth.KindAPIKey).
	KindAPIKey Kind = "api_key"
)

// Model is one catalog entry: the id to send to the provider, the provider
// serving it, gofer's short display label, and the context window when known.
//
// Label prefers the live listing's display_name and falls back to
// [modelmeta.DisplayName] (which itself falls back to the raw id), so it is
// never blank. ContextWindow is 0 when unknown — which means UNKNOWN, not "no
// context", exactly as provider.ModelInfo documents; a renderer must not
// divide by it or present it as a limit.
//
// There is deliberately no pricing member: the live listing carries no price
// because subscription models have none. See the package doc.
type Model struct {
	ID            string
	Provider      string
	Label         string
	ContextWindow int
}

const (
	providerOpenAI = "openai"

	// codexDefaultModel is the default for an OpenAI OAuth (ChatGPT
	// subscription) credential.
	//
	// Chosen as the balanced everyday tier of the Codex family, not its
	// frontier tier: the vendor describes gpt-5.6-sol as the frontier agentic
	// coding model, gpt-5.6-terra as the balanced everyday one, and
	// gpt-5.6-luna as the fast/cheap one. That mirrors what gofer already
	// defaults to on the other vendor — claude-sonnet-5, the mid workhorse,
	// rather than the flagship — and a default is what runs before the user has
	// expressed any preference, so it should spend the least of a subscription's
	// finite quota that still does the job. Users who want the frontier tier say
	// so via -m, /model, or session.model in config.json, all of which outrank
	// this.
	//
	// An older, more conservative id (gpt-5.5) was considered on the theory that
	// newer models may be gated behind a minimum client version. That gate is
	// real in the vendor CLI's static catalog but does not bind here: the live
	// listing that this table is transcribed from carries no minimum-version
	// field on any entry, and it listed the whole 5.6 family as visible. The
	// version gate is also a property of THAT client, which gofer is not. So the
	// current-generation balanced tier wins; if a 5.6 id is ever refused for a
	// client-version reason, gpt-5.5 is the fallback to move to.
	codexDefaultModel = "gpt-5.6-terra"
)

// codexModels is the model family the Codex backend serves, in the order the
// picker lists them. See the package doc for why this is compiled in, why it is
// a fallback rather than the end state, and why it is not an admission gate.
//
// Provenance: the set is transcribed from a cached response of the Codex
// backend's own models listing (schema: slug, display_name, visibility),
// observed 2026-07 against client version 0.144.3. That listing carries a
// visibility field, and only its list-visible entries appear here — the
// hide-visible ones (gpt-5.4, gpt-5.4-mini, and the codex-auto-review internal
// review model) are omitted deliberately, since the backend itself marks them
// as not for a model picker. Omission is not exclusion: a user can still name
// any of them, because this table gates nothing.
//
// One id needs care if this is ever revised. The vendor CLI's own compiled-in
// FALLBACK catalog disagrees with the live listing above: it omits
// gpt-5.3-codex-spark and carries a list-visible gpt-5.2. The live listing is
// the better authority — it is what the backend actually served, and the CLI's
// static file is by construction the stale path — so gpt-5.3-codex-spark is
// included and gpt-5.2 is not. Both are worth re-checking against a fresh
// listing whenever this table is touched.
var codexModels = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.3-codex-spark",
}

// DefaultModelForKind returns the model id gofer defaults to for providerID
// under a credential of kind, with no IO of any sort.
//
// Every combination except OpenAI+OAuth delegates to runner.DefaultModel, so
// Anthropic (either kind) and OpenAI+API-key keep exactly the SDK's answer and
// an unknown provider keeps the SDK's empty-string miss. OpenAI+OAuth routes to
// the Codex backend, which rejects the SDK's gpt-5, and yields
// [codexDefaultModel] instead.
func DefaultModelForKind(providerID string, kind Kind) string {
	if providerID == providerOpenAI && kind == KindOAuth {
		return codexDefaultModel
	}
	return runner.DefaultModel(providerID)
}

// CatalogForKind returns the models providerID's kind of credential can reach,
// in display order, with no IO of any sort. It returns nil for a provider with
// no known models rather than an error — an empty picker list, never a blocked
// view.
//
// OpenAI+OAuth returns the Codex family ([codexModels]), which the SDK registry
// does not carry. Everything else is the SDK registry filtered to providerID
// and sorted by id — delegated, never duplicated, so a model added to the SDK
// registry shows up here with no change to this package.
func CatalogForKind(providerID string, kind Kind) []Model {
	if providerID == providerOpenAI && kind == KindOAuth {
		return codexFloor()
	}

	ids := provider.Models()
	out := make([]Model, 0, len(ids))
	for _, id := range ids {
		info, ok := provider.Lookup(id)
		if !ok || info.Provider != providerID {
			continue
		}
		out = append(out, Model{
			ID:            id,
			Provider:      info.Provider,
			Label:         modelmeta.DisplayName(id),
			ContextWindow: info.ContextWindow,
		})
	}
	if len(out) == 0 {
		return nil
	}
	// provider.Models() is unordered; sort so the picker's list is stable.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// codexFloor is the static Codex family as [Model]s — the offline floor. Its
// context windows are deliberately 0 (unknown): the real values come from the
// live listing, and inventing them here would be the same class of error as
// inventing a price.
func codexFloor() []Model {
	out := make([]Model, 0, len(codexModels))
	for _, id := range codexModels {
		out = append(out, Model{ID: id, Provider: providerOpenAI, Label: modelmeta.DisplayName(id)})
	}
	return out
}

// CredentialKind reports the kind of credential configured for providerID under
// root (gofer's resolved store root, e.g. supervisor.ResolveRoot's result).
//
// It performs no network IO: the kind is read from auth.json via
// auth.Store.Get, deliberately not auth.Store.Credential, which would refresh an
// expired OAuth token over the network (see the package doc).
//
// A provider with no stored entry but a populated env var
// (runner.EnvVar(providerID), e.g. OPENAI_API_KEY) reports [KindAPIKey] —
// that path is how runner.CredentialedProviders counts it as credentialed, and
// an env var is by construction an API key. With neither, it reports
// [auth.ErrNoCredential] wrapped with the provider id, which callers can test
// with errors.Is.
func CredentialKind(ctx context.Context, root, providerID string) (Kind, error) {
	// Honor cancellation before touching the filesystem: this runs on the
	// startup path of every command, where the caller's context may already be
	// done.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	store, err := auth.New(auth.WithRoot(root))
	if err != nil {
		return "", fmt.Errorf("open auth store: %w", err)
	}
	entry, ok, err := store.Get(providerID)
	if err != nil {
		return "", fmt.Errorf("read %s credential: %w", providerID, err)
	}
	if ok {
		return Kind(entry.Kind), nil
	}
	if env := runner.EnvVar(providerID); env != "" && os.Getenv(env) != "" {
		return KindAPIKey, nil
	}
	return "", fmt.Errorf("%w: %s", auth.ErrNoCredential, providerID)
}

// DefaultModelIn validates — and if necessary upgrades — the credential-kind
// default against a catalog the caller already has in hand (typically a live
// one from [Catalog]). It returns the kind-appropriate default when that model
// is actually present, and otherwise the first entry of the catalog.
//
// This is the discovery half of the default decision, deliberately split from
// [DefaultModel] so that correctness-by-discovery costs a caller who already
// paid for a listing nothing extra, while the startup path stays offline. An
// empty catalog changes nothing: the constant stands, since an absent listing
// is no evidence that the constant is wrong.
func DefaultModelIn(providerID string, kind Kind, catalog []Model) string {
	want := DefaultModelForKind(providerID, kind)
	if len(catalog) == 0 {
		return want
	}
	for _, m := range catalog {
		if m.ID == want {
			return want
		}
	}
	// The compiled-in default is not in the list the credential can actually
	// reach — it was retired or renamed. The catalog is ordered most to least
	// capable, so its head is the best available stand-in.
	return catalog[0].ID
}

// DefaultModel returns the model id gofer defaults to for providerID, resolving
// the credential kind under root itself.
//
// It performs NO network call, by design, and that is the deliberate half of
// the default decision. Discovery would make the default correct by
// construction, but this runs on the startup path of every `gofer run`, and
// paying a vendor round-trip — plus its timeout on a flaky network — before the
// first prompt is a bad trade for a value that is right today and changes about
// as often as a model generation. A constant is also deterministic: the same
// invocation resolves the same model whether or not the network is up, which is
// what makes gofer's behavior reproducible and its tests hermetic.
//
// Staleness is the real cost, and it is covered where it is cheap rather than
// where it is expensive: [DefaultModelIn] upgrades the constant against a
// catalog a caller already fetched, so the picker — which is fetching a live
// listing anyway — self-corrects. A caller that wants a discovery-validated
// default without a picker can combine the two:
//
//	cat, _ := modelcatalog.Catalog(ctx, root, id, modelcatalog.WithDiscovery(fn))
//	model := modelcatalog.DefaultModelIn(id, kind, cat)
//
// A provider with no credential at all is not an error here: it falls back to
// the SDK's provider-keyed default, so a caller resolving a model for a
// not-yet-logged-in provider still gets a sensible id and fails later, at
// runner construction, with the credential error that names the provider. Only
// a genuinely broken store (unreadable/malformed auth.json) is returned as an
// error.
func DefaultModel(ctx context.Context, root, providerID string) (string, error) {
	kind, err := credentialKindOrNone(ctx, root, providerID)
	if err != nil {
		return "", err
	}
	return DefaultModelForKind(providerID, kind), nil
}

// Catalog returns the models providerID's configured credential can reach, in
// display order, resolving the credential kind under root itself.
//
// With [WithDiscovery] set and an OpenAI OAuth credential, it asks the Codex
// backend for the live listing (bounded by [WithTimeout] /
// [DefaultDiscoveryTimeout]) and returns that. Every failure — discovery not
// enabled, wrong credential kind, expired credential, HTTP 400/401, a malformed
// body, a transport error, a timeout, or a listing with nothing selectable in
// it — falls back to the static floor. Discovery failure is NEVER surfaced as
// an error and NEVER produces an empty list: an offline user gets a
// stale-but-usable picker, which is the whole point of the floor. Only a broken
// auth store (unreadable/malformed auth.json) is an error, and only because
// guessing past it would hide a real failure.
//
// Without [WithDiscovery] this is pure and offline, identical to
// [CatalogForKind] — see that option's doc for why live is opt-in.
func Catalog(ctx context.Context, root, providerID string, opts ...Option) ([]Model, error) {
	kind, err := credentialKindOrNone(ctx, root, providerID)
	if err != nil {
		return nil, err
	}
	o := newOptions(opts)
	if o.discover && providerID == providerOpenAI && kind == KindOAuth {
		if live, derr := discoverCodex(ctx, root, o); derr == nil {
			return live, nil
		}
		// Deliberately swallowed: see the doc above. The floor below is the
		// designed response to every discovery failure, so there is no error to
		// propagate and nothing a caller could do differently with one.
	}
	return CatalogForKind(providerID, kind), nil
}

// credentialKindOrNone is CredentialKind with "no credential configured"
// softened to the empty Kind — which both pure entry points treat as the
// non-OAuth (registry) view. Every other error, notably an unreadable or
// malformed auth.json, still propagates: guessing past a broken store would
// hide the real failure.
func credentialKindOrNone(ctx context.Context, root, providerID string) (Kind, error) {
	kind, err := CredentialKind(ctx, root, providerID)
	switch {
	case err == nil:
		return kind, nil
	case errors.Is(err, auth.ErrNoCredential):
		return "", nil
	default:
		return "", err
	}
}
