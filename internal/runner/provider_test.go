package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/auth"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// TestNewProvider_UnknownModel asserts a legible, hermetic (no network)
// error for a model the SDK registry does not recognize.
func TestNewProvider_UnknownModel(t *testing.T) {
	_, err := newProvider(context.Background(), "not-a-real-model", t.TempDir())
	if err == nil {
		t.Fatal("newProvider: got nil error, want an unknown-model error")
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Errorf("newProvider error = %q, want it to mention the unknown model", err.Error())
	}
}

// TestCompositeCredSource_EnvFallback asserts the composite falls back to the
// environment when the auth store has no entry for the provider, and that
// the combined error is legible when neither source has one.
func TestCompositeCredSource_EnvFallback(t *testing.T) {
	store, err := auth.New(auth.WithRoot(t.TempDir()))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	t.Run("falls back to env", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
		src := compositeCredSource{store: store, env: provider.StaticEnv()}

		cred, err := src.Credential(context.Background(), "anthropic")
		if err != nil {
			t.Fatalf("Credential: %v", err)
		}
		if cred.Token != "sk-test-key" {
			t.Errorf("cred.Token = %q, want %q", cred.Token, "sk-test-key")
		}
	})

	t.Run("legible error when neither source has a credential", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		src := compositeCredSource{store: store, env: provider.StaticEnv()}

		_, err := src.Credential(context.Background(), "anthropic")
		if err == nil {
			t.Fatal("Credential: got nil error, want one")
		}
		// One short, actionable sentence — not the redundant wrapped chain.
		want := "no credential for anthropic — run 'gofer login anthropic' or set ANTHROPIC_API_KEY"
		if err.Error() != want {
			t.Errorf("Credential error = %q, want %q", err.Error(), want)
		}
		// errors.Is matches the sentinel, and the underlying causes are retained
		// via Unwrap for --json / structured consumers.
		if !errors.Is(err, ErrNoCredential) {
			t.Errorf("error is not ErrNoCredential: %v", err)
		}
		if !errors.Is(err, auth.ErrNoCredential) {
			t.Errorf("error lost the underlying auth.ErrNoCredential cause: %v", err)
		}
	})
}
