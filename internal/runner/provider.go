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

// newProvider resolves model's backend from the SDK's model registry and
// builds a real provider.Provider over it, authenticated by a
// compositeCredSource (gofer login OAuth/API-key first, an environment
// variable second).
func newProvider(model, root string) (provider.Provider, error) {
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
// auth.Store first — the material a `gofer login` OAuth flow or `gofer auth
// set-key` would have written — and falls back to the provider's
// conventional environment variable when the store has no entry for it. This
// lets `gofer run`/`gofer resume` work with either path with no extra flags.
type compositeCredSource struct {
	store *auth.Store
	env   provider.CredentialSource
}

var _ provider.CredentialSource = compositeCredSource{}

// Credential implements provider.CredentialSource.
func (c compositeCredSource) Credential(ctx context.Context, providerID string) (provider.Credential, error) {
	cred, err := c.store.Credential(ctx, providerID)
	if err == nil {
		return cred, nil
	}
	if !errors.Is(err, auth.ErrNoCredential) {
		return provider.Credential{}, fmt.Errorf("runner: resolve %s credential: %w", providerID, err)
	}

	cred, envErr := c.env.Credential(ctx, providerID)
	if envErr != nil {
		return provider.Credential{}, fmt.Errorf(
			"no credential for %s — run 'gofer login %s' or set its API key environment variable: %w",
			providerID, providerID, envErr)
	}
	return cred, nil
}
