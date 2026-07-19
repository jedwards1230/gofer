package modelcatalog_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/auth"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/modelcatalog"
)

// TestKindsMirrorSDK locks gofer's Kind constants to the SDK's auth.CredKind
// values. The whole package hinges on Kind(entry.Kind) round-tripping, so a
// silent rename on either side must fail here rather than downgrade every
// OAuth user to the API-key catalog.
func TestKindsMirrorSDK(t *testing.T) {
	if got, want := string(modelcatalog.KindOAuth), string(auth.KindOAuth); got != want {
		t.Errorf("KindOAuth = %q, want auth.KindOAuth (%q)", got, want)
	}
	if got, want := string(modelcatalog.KindAPIKey), string(auth.KindAPIKey); got != want {
		t.Errorf("KindAPIKey = %q, want auth.KindAPIKey (%q)", got, want)
	}
}

// TestDefaultModelForKind is the core of issue #157: the default depends on
// credential kind, not provider id alone. Only OpenAI+OAuth diverges from the
// SDK's answer.
func TestDefaultModelForKind(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		kind     modelcatalog.Kind
		want     string
	}{
		// The bug: gpt-5 is rejected with HTTP 400 on the Codex backend an
		// OAuth credential routes to.
		{"openai oauth uses the codex family", "openai", modelcatalog.KindOAuth, "gpt-5.6-terra"},
		// No regression for the API-key path — still the SDK's registry family.
		{"openai api key unchanged", "openai", modelcatalog.KindAPIKey, "gpt-5"},
		// Anthropic serves one family on both kinds; neither may regress.
		{"anthropic oauth unchanged", "anthropic", modelcatalog.KindOAuth, "claude-sonnet-5"},
		{"anthropic api key unchanged", "anthropic", modelcatalog.KindAPIKey, "claude-sonnet-5"},
		// An unknown kind (an auth.json from a newer gofer) must not silently
		// become the Codex family.
		{"openai unknown kind falls back to the sdk default", "openai", modelcatalog.Kind("carrier-pigeon"), "gpt-5"},
		{"unknown provider misses like the sdk", "widget", modelcatalog.KindOAuth, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelcatalog.DefaultModelForKind(tt.provider, tt.kind); got != tt.want {
				t.Errorf("DefaultModelForKind(%q, %q) = %q, want %q", tt.provider, tt.kind, got, tt.want)
			}
		})
	}
}

// TestCatalogForKind covers the picker's data per credential kind: the Codex
// family for OpenAI+OAuth, and the SDK registry (delegated, not duplicated)
// for every other combination.
func TestCatalogForKind(t *testing.T) {
	t.Run("openai oauth lists the codex family in order", func(t *testing.T) {
		want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.3-codex-spark"}
		got := ids(modelcatalog.CatalogForKind("openai", modelcatalog.KindOAuth))
		if !slices.Equal(got, want) {
			t.Errorf("CatalogForKind(openai, oauth) = %v, want %v", got, want)
		}
		// gpt-5 is exactly the id the backend rejects — it must not be offered.
		if slices.Contains(got, "gpt-5") {
			t.Error("CatalogForKind(openai, oauth) offers gpt-5, which the Codex backend rejects with HTTP 400")
		}
	})

	t.Run("openai oauth entries carry provider and label", func(t *testing.T) {
		for _, m := range modelcatalog.CatalogForKind("openai", modelcatalog.KindOAuth) {
			if m.Provider != "openai" {
				t.Errorf("model %q Provider = %q, want openai", m.ID, m.Provider)
			}
			if m.Label == "" || m.Label == m.ID {
				t.Errorf("model %q Label = %q, want a display name distinct from the raw id", m.ID, m.Label)
			}
		}
	})

	t.Run("openai api key delegates to the sdk registry", func(t *testing.T) {
		got := ids(modelcatalog.CatalogForKind("openai", modelcatalog.KindAPIKey))
		want := registryIDs("openai")
		if !slices.Equal(got, want) {
			t.Errorf("CatalogForKind(openai, api_key) = %v, want the SDK registry's openai family %v", got, want)
		}
		if !slices.Contains(got, "gpt-5") {
			t.Errorf("CatalogForKind(openai, api_key) = %v, want it to still carry gpt-5", got)
		}
	})

	t.Run("anthropic is the sdk registry on both kinds", func(t *testing.T) {
		want := registryIDs("anthropic")
		for _, kind := range []modelcatalog.Kind{modelcatalog.KindOAuth, modelcatalog.KindAPIKey} {
			got := ids(modelcatalog.CatalogForKind("anthropic", kind))
			if !slices.Equal(got, want) {
				t.Errorf("CatalogForKind(anthropic, %q) = %v, want %v", kind, got, want)
			}
		}
	})

	t.Run("unknown provider yields an empty list, not an error path", func(t *testing.T) {
		if got := modelcatalog.CatalogForKind("widget", modelcatalog.KindAPIKey); got != nil {
			t.Errorf("CatalogForKind(widget, api_key) = %v, want nil", got)
		}
	})
}

// TestCatalogIsNotAnAdmissionGate is the load-bearing non-gate proof. The
// hardcoded Codex table exists to display and to default — never to decide
// which ids a user may run. An id on no list of gofer's must stay runnable, so
// the compiled-in catalog can never be the reason a newly released model is
// unreachable. If this ever fails, the table has become a gate and the
// hardcoding is no longer defensible.
func TestCatalogIsNotAnAdmissionGate(t *testing.T) {
	// Deliberately absent from BOTH the Codex table and the SDK registry.
	const unlisted = "gpt-9.9-unreleased-tomorrow"

	for _, kind := range []modelcatalog.Kind{modelcatalog.KindOAuth, modelcatalog.KindAPIKey} {
		if slices.Contains(ids(modelcatalog.CatalogForKind("openai", kind)), unlisted) {
			t.Fatalf("test premise broken: %q is listed for kind %q", unlisted, kind)
		}
	}
	if _, ok := provider.Lookup(unlisted); ok {
		t.Fatalf("test premise broken: %q is in the SDK registry", unlisted)
	}

	// The actual admission decision belongs to provider.Resolve, which admits
	// an unregistered id by inferring its backend from the id's shape. Being
	// absent from the catalog changes nothing about it.
	info, err := provider.Resolve(unlisted)
	if err != nil {
		t.Fatalf("provider.Resolve(%q) = %v, want an unlisted id to still be runnable", unlisted, err)
	}
	if info.Provider != "openai" {
		t.Errorf("provider.Resolve(%q).Provider = %q, want openai", unlisted, info.Provider)
	}

	// And nothing in this package rejects, rewrites, or filters such an id:
	// the catalog is data the caller reads, not a check the caller passes.
	// There is intentionally no modelcatalog.Validate/Allow/Contains to call.
}

// TestCredentialKind covers reading the persisted kind off disk — the seam the
// root-taking entry points hang on. Every case is hermetic: a t.TempDir() root
// and both provider env vars cleared. No vendor host is contacted, and none
// can be: auth.Store.Get is a plain read of auth.json (Store.Credential, which
// can refresh an OAuth token over the network, is deliberately not used).
func TestCredentialKind(t *testing.T) {
	ctx := context.Background()

	t.Run("stored oauth entry", func(t *testing.T) {
		root := hermeticRoot(t)
		writeCredential(t, root, "openai", auth.Entry{Kind: auth.KindOAuth, Access: "tok", Refresh: "ref"})

		got, err := modelcatalog.CredentialKind(ctx, root, "openai")
		if err != nil {
			t.Fatalf("CredentialKind: %v", err)
		}
		if got != modelcatalog.KindOAuth {
			t.Errorf("CredentialKind = %q, want %q", got, modelcatalog.KindOAuth)
		}
	})

	t.Run("stored api key entry", func(t *testing.T) {
		root := hermeticRoot(t)
		writeCredential(t, root, "openai", auth.Entry{Kind: auth.KindAPIKey, Access: "sk-test"})

		got, err := modelcatalog.CredentialKind(ctx, root, "openai")
		if err != nil {
			t.Fatalf("CredentialKind: %v", err)
		}
		if got != modelcatalog.KindAPIKey {
			t.Errorf("CredentialKind = %q, want %q", got, modelcatalog.KindAPIKey)
		}
	})

	// An env var is by construction an API key, and it is how
	// runner.CredentialedProviders counts a provider as credentialed with no
	// store entry at all.
	t.Run("env var with no stored entry is an api key", func(t *testing.T) {
		root := hermeticRoot(t)
		t.Setenv("OPENAI_API_KEY", "sk-test")

		got, err := modelcatalog.CredentialKind(ctx, root, "openai")
		if err != nil {
			t.Fatalf("CredentialKind: %v", err)
		}
		if got != modelcatalog.KindAPIKey {
			t.Errorf("CredentialKind = %q, want %q", got, modelcatalog.KindAPIKey)
		}
	})

	t.Run("no credential at all", func(t *testing.T) {
		root := hermeticRoot(t)

		_, err := modelcatalog.CredentialKind(ctx, root, "openai")
		if !errors.Is(err, auth.ErrNoCredential) {
			t.Errorf("CredentialKind err = %v, want auth.ErrNoCredential", err)
		}
	})

	t.Run("cancelled context is honored before any filesystem read", func(t *testing.T) {
		root := hermeticRoot(t)
		writeCredential(t, root, "openai", auth.Entry{Kind: auth.KindOAuth, Access: "tok"})
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := modelcatalog.CredentialKind(cancelled, root, "openai"); !errors.Is(err, context.Canceled) {
			t.Errorf("CredentialKind err = %v, want context.Canceled", err)
		}
	})

	t.Run("malformed auth.json is an error, never a guess", func(t *testing.T) {
		root := hermeticRoot(t)
		if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write auth.json: %v", err)
		}

		if _, err := modelcatalog.CredentialKind(ctx, root, "openai"); err == nil {
			t.Error("CredentialKind: got nil error for a malformed auth.json, want an error")
		}
	})
}

// TestDefaultModel covers the root-taking entry point end to end: the same
// per-kind answers as TestDefaultModelForKind, but with the kind read off disk.
func TestDefaultModel(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		provider string
		entry    *auth.Entry // nil = no stored credential
		want     string
	}{
		{"openai oauth", "openai", &auth.Entry{Kind: auth.KindOAuth, Access: "tok"}, "gpt-5.6-terra"},
		{"openai api key", "openai", &auth.Entry{Kind: auth.KindAPIKey, Access: "sk-test"}, "gpt-5"},
		{"anthropic oauth", "anthropic", &auth.Entry{Kind: auth.KindOAuth, Access: "tok"}, "claude-sonnet-5"},
		{"anthropic api key", "anthropic", &auth.Entry{Kind: auth.KindAPIKey, Access: "sk-test"}, "claude-sonnet-5"},
		// No credential is not an error: the SDK's provider-keyed default
		// still resolves and the credential error surfaces later, where it
		// names the provider.
		{"openai with no credential falls back to the sdk default", "openai", nil, "gpt-5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := hermeticRoot(t)
			if tt.entry != nil {
				writeCredential(t, root, tt.provider, *tt.entry)
			}

			got, err := modelcatalog.DefaultModel(ctx, root, tt.provider)
			if err != nil {
				t.Fatalf("DefaultModel: %v", err)
			}
			if got != tt.want {
				t.Errorf("DefaultModel(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}

	t.Run("broken store propagates", func(t *testing.T) {
		root := hermeticRoot(t)
		if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write auth.json: %v", err)
		}

		if _, err := modelcatalog.DefaultModel(ctx, root, "openai"); err == nil {
			t.Error("DefaultModel: got nil error for a malformed auth.json, want an error")
		}
	})
}

// TestCatalog covers the root-taking catalog: an OAuth credential on disk
// yields the Codex family, an API key yields the registry family.
func TestCatalog(t *testing.T) {
	ctx := context.Background()

	t.Run("stored oauth credential yields the codex family", func(t *testing.T) {
		root := hermeticRoot(t)
		writeCredential(t, root, "openai", auth.Entry{Kind: auth.KindOAuth, Access: "tok"})

		got, err := modelcatalog.Catalog(ctx, root, "openai")
		if err != nil {
			t.Fatalf("Catalog: %v", err)
		}
		want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.3-codex-spark"}
		if !slices.Equal(ids(got), want) {
			t.Errorf("Catalog(openai) = %v, want %v", ids(got), want)
		}
	})

	t.Run("stored api key yields the registry family", func(t *testing.T) {
		root := hermeticRoot(t)
		writeCredential(t, root, "openai", auth.Entry{Kind: auth.KindAPIKey, Access: "sk-test"})

		got, err := modelcatalog.Catalog(ctx, root, "openai")
		if err != nil {
			t.Fatalf("Catalog: %v", err)
		}
		if !slices.Equal(ids(got), registryIDs("openai")) {
			t.Errorf("Catalog(openai) = %v, want the SDK registry's openai family %v", ids(got), registryIDs("openai"))
		}
	})
}

// hermeticRoot returns a fresh store root with both provider env vars cleared,
// so a developer's real ANTHROPIC_API_KEY / OPENAI_API_KEY in the ambient
// environment cannot change an assertion.
func hermeticRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	return t.TempDir()
}

// writeCredential persists one provider entry into root's auth.json through
// the SDK's own store, so the on-disk shape is whatever the SDK writes rather
// than a hand-rolled fixture that could drift from it. Store.Set performs no
// network IO.
func writeCredential(t *testing.T, root, providerID string, e auth.Entry) {
	t.Helper()
	store, err := auth.New(auth.WithRoot(root))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if err := store.Set(providerID, e); err != nil {
		t.Fatalf("store.Set(%s): %v", providerID, err)
	}
}

// ids flattens a catalog to its model ids, preserving order.
func ids(models []modelcatalog.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// registryIDs is the SDK registry's models for a provider, sorted by id — the
// expectation the non-Codex catalog paths must equal, computed from the
// registry itself so adding a model to the SDK never fails these tests.
func registryIDs(providerID string) []string {
	var out []string
	for _, id := range provider.Models() {
		if info, ok := provider.Lookup(id); ok && info.Provider == providerID {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
