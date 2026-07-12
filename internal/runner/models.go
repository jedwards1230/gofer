package runner

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/jedwards1230/agent-sdk-go/auth"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"
)

// supportedProviders are the provider ids gofer knows how to run against, in
// a fixed, deterministic (alphabetical) order. Both the credential-presence
// probe and the default-model table below are keyed off this list, so gofer
// never favors one vendor over another — there is no flagship default.
var supportedProviders = []string{"anthropic", "openai"}

// SupportedProviders returns the provider ids gofer knows how to run
// against, in the same fixed order supportedProviders defines.
func SupportedProviders() []string {
	out := make([]string, len(supportedProviders))
	copy(out, supportedProviders)
	return out
}

// defaultModels is the model gofer runs when a caller doesn't pass -m and
// exactly one provider has a credential configured.
var defaultModels = map[string]string{
	"anthropic": "claude-sonnet-5",
	"openai":    "gpt-5",
}

// DefaultModel returns providerID's default model, or "" for an unknown
// provider id.
func DefaultModel(providerID string) string { return defaultModels[providerID] }

// EnvVar returns the API-key environment variable gofer's credential
// resolution falls back to for providerID, or "" for an unknown provider id.
// It is the same envVars mapping newProvider's compositeCredSource consumes,
// so the checked variable and any hint built from it can never drift.
func EnvVar(providerID string) string { return envVars[providerID] }

// CredentialedProviders returns the subset of SupportedProviders that
// currently have some credential configured — either a stored `gofer login`
// entry or the provider's API-key environment variable — sorted in
// SupportedProviders order. It is a cheap existence probe only:
// auth.Store.Get is a lock-free local read (no network call, no OAuth
// refresh), so it is safe to call before every run to pick a default model.
// The actual credential resolution (including any OAuth refresh, or a
// stored-but-unresolvable credential) still happens later, in newProvider's
// pre-flight. ctx is accepted (and currently unused) for symmetry with
// Transcript and the rest of the package's context-taking API.
func CredentialedProviders(_ context.Context, root string) ([]string, error) {
	var authOpts []auth.Option
	if root != "" {
		authOpts = append(authOpts, auth.WithRoot(root))
	}
	store, err := auth.New(authOpts...)
	if err != nil {
		return nil, fmt.Errorf("runner: open credential store: %w", err)
	}

	var out []string
	for _, id := range supportedProviders {
		if _, ok, err := store.Get(id); err != nil {
			return nil, fmt.Errorf("runner: read %s credential: %w", id, err)
		} else if ok {
			out = append(out, id)
			continue
		}
		if os.Getenv(envVars[id]) != "" {
			out = append(out, id)
		}
	}
	// supportedProviders is already sorted; sort explicitly so the guarantee
	// doesn't silently depend on that fact staying true.
	sort.Strings(out)
	return out, nil
}

// Transcript returns the folded provider messages for session id, read
// directly from its journal. It builds no provider and resolves no
// credential, so a transcript view works with nothing configured — unlike
// Resume, which pre-flights a credential for the (possibly unused) case
// where the caller goes on to continue the session with a prompt.
func Transcript(ctx context.Context, id, root string) ([]provider.Message, error) {
	var storeOpts []session.StoreOption
	if root != "" {
		storeOpts = append(storeOpts, session.WithRoot(root))
	}
	store, err := session.NewFileStore(storeOpts...)
	if err != nil {
		return nil, fmt.Errorf("runner: open session store: %w", err)
	}
	defer func() { _ = store.Close() }()

	journal, err := store.Open(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("runner: open session %s: %w", id, err)
	}
	defer func() { _ = journal.Close() }()

	return journal.Fold(), nil
}
