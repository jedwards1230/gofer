package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/jedwards1230/agent-sdk-go/auth"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/anthropic"
	"github.com/jedwards1230/agent-sdk-go/provider/openai"
)

// ErrNoCredential marks a run that cannot start because no credential is
// configured for its provider (neither a stored gofer login nor the provider's
// API-key environment variable). Callers can errors.Is against it; the wrapped
// cause carries the underlying resolution failures for structured consumers.
var ErrNoCredential = errors.New("no credential configured")

// credentialError is a missing-credential error with a single short, actionable
// message. It keeps the underlying resolution errors reachable via Unwrap (so
// --json / errors.Is consumers lose nothing) while Error() stays one sentence
// rather than the redundant wrapped chain.
type credentialError struct {
	provider string
	envVar   string
	cause    error
}

func (e *credentialError) Error() string {
	if e.envVar == "" {
		return fmt.Sprintf("no credential for %s — run 'gofer login %s'", e.provider, e.provider)
	}
	return fmt.Sprintf("no credential for %s — run 'gofer login %s' or set %s", e.provider, e.provider, e.envVar)
}

func (e *credentialError) Unwrap() error { return e.cause }

// Is reports a match against the ErrNoCredential sentinel; the wrapped cause
// still satisfies errors.Is for the SDK-level errors (e.g. auth.ErrNoCredential).
func (e *credentialError) Is(target error) bool { return target == ErrNoCredential }

// newProvider resolves model's backend from the SDK's model registry, builds a
// compositeCredSource (gofer login OAuth/API-key first, an environment variable
// second), PRE-FLIGHTS the credential (a store/env lookup — not a live model API
// call — so a missing credential fails before any session journal is created),
// and returns a real provider.Provider over it.
func newProvider(ctx context.Context, model, root string) (provider.Provider, error) {
	info, ok := provider.Lookup(model)
	if !ok {
		return nil, fmt.Errorf("runner: unknown model %q", model)
	}

	var authOpts []auth.Option
	if root != "" {
		authOpts = append(authOpts, auth.WithRoot(root))
	}
	store, err := auth.New(authOpts...)
	if err != nil {
		return nil, fmt.Errorf("runner: open credential store: %w", err)
	}
	creds := compositeCredSource{store: store, env: provider.StaticEnv()}

	// Pre-flight the credential so a misconfiguration fails fast with no orphan
	// journal. A credential that resolves here but is rejected by the live API
	// still runs as a real errored session (that resolution succeeds).
	if _, err := creds.Credential(ctx, info.Provider); err != nil {
		return nil, err
	}

	switch info.Provider {
	case "anthropic":
		return anthropic.New(model, creds), nil
	case "openai":
		return openai.New(model, creds), nil
	default:
		return nil, fmt.Errorf("runner: unsupported provider %q for model %q", info.Provider, model)
	}
}

// compositeCredSource resolves a provider credential from the persisted
// auth.Store first — the material a `gofer login` OAuth flow or `gofer login
// <provider> --api-key` would have written — and falls back to the provider's
// conventional environment variable when the store has no entry for it. This
// lets `gofer run`/`gofer resume` work with either path with no extra flags.
type compositeCredSource struct {
	store *auth.Store
	env   provider.EnvCredentialSource
}

var _ provider.CredentialSource = compositeCredSource{}

// Credential implements provider.CredentialSource.
func (c compositeCredSource) Credential(ctx context.Context, providerID string) (provider.Credential, error) {
	cred, err := c.store.Credential(ctx, providerID)
	if err == nil {
		return cred, nil
	}
	if !errors.Is(err, auth.ErrNoCredential) {
		// A credential exists in the store but could not be resolved (e.g. an
		// expired OAuth token that failed to refresh) — surface it verbatim; it
		// is a resolution failure, not a missing credential.
		return provider.Credential{}, fmt.Errorf("runner: resolve %s credential: %w", providerID, err)
	}

	cred, envErr := c.env.Credential(ctx, providerID)
	if envErr == nil {
		return cred, nil
	}
	return provider.Credential{}, &credentialError{
		provider: providerID,
		envVar:   c.env.Vars[providerID],
		cause:    errors.Join(err, envErr),
	}
}
