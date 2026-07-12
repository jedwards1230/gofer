package runner_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/auth"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/runner"
)

// TestSupportedProviders_DefaultModel_EnvVar locks the static provider
// tables: a fixed, deterministic (alphabetical) provider list, and the
// default model / env var each one resolves to. gofer ships with no
// flagship-vendor default, so both providers must be represented.
func TestSupportedProviders_DefaultModel_EnvVar(t *testing.T) {
	got := runner.SupportedProviders()
	want := []string{"anthropic", "openai"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SupportedProviders() = %v, want %v", got, want)
	}

	// A caller mutating the returned slice must not affect later calls.
	got[0] = "mutated"
	if again := runner.SupportedProviders(); !reflect.DeepEqual(again, want) {
		t.Fatalf("SupportedProviders() after caller mutation = %v, want %v (want a copy)", again, want)
	}

	if m := runner.DefaultModel("anthropic"); m != "claude-sonnet-5" {
		t.Errorf("DefaultModel(anthropic) = %q, want claude-sonnet-5", m)
	}
	if m := runner.DefaultModel("openai"); m != "gpt-5" {
		t.Errorf("DefaultModel(openai) = %q, want gpt-5", m)
	}
	if m := runner.DefaultModel("bogus"); m != "" {
		t.Errorf("DefaultModel(bogus) = %q, want empty", m)
	}

	if v := runner.EnvVar("anthropic"); v != "ANTHROPIC_API_KEY" {
		t.Errorf("EnvVar(anthropic) = %q, want ANTHROPIC_API_KEY", v)
	}
	if v := runner.EnvVar("openai"); v != "OPENAI_API_KEY" {
		t.Errorf("EnvVar(openai) = %q, want OPENAI_API_KEY", v)
	}
	if v := runner.EnvVar("bogus"); v != "" {
		t.Errorf("EnvVar(bogus) = %q, want empty", v)
	}
}

// TestCredentialedProviders covers every combination of store/env presence
// for the two supported providers. It is hermetic: t.Setenv always sets both
// vars explicitly (including to "") so the outcome never depends on the
// ambient environment, and the auth store is rooted at a fresh t.TempDir().
func TestCredentialedProviders(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "")

		got, err := runner.CredentialedProviders(context.Background(), root)
		if err != nil {
			t.Fatalf("CredentialedProviders: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("CredentialedProviders = %v, want none", got)
		}
	})

	t.Run("only env", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
		t.Setenv("OPENAI_API_KEY", "")

		got, err := runner.CredentialedProviders(context.Background(), root)
		if err != nil {
			t.Fatalf("CredentialedProviders: %v", err)
		}
		want := []string{"anthropic"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CredentialedProviders = %v, want %v", got, want)
		}
	})

	t.Run("only store", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "")

		store, err := auth.New(auth.WithRoot(root))
		if err != nil {
			t.Fatalf("auth.New: %v", err)
		}
		if err := store.SetAPIKey("openai", "sk-openai-fake"); err != nil {
			t.Fatalf("SetAPIKey: %v", err)
		}

		got, err := runner.CredentialedProviders(context.Background(), root)
		if err != nil {
			t.Fatalf("CredentialedProviders: %v", err)
		}
		want := []string{"openai"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CredentialedProviders = %v, want %v", got, want)
		}
	})

	t.Run("both", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "sk-openai-env")

		store, err := auth.New(auth.WithRoot(root))
		if err != nil {
			t.Fatalf("auth.New: %v", err)
		}
		if err := store.SetAPIKey("anthropic", "sk-anthropic-fake"); err != nil {
			t.Fatalf("SetAPIKey: %v", err)
		}

		got, err := runner.CredentialedProviders(context.Background(), root)
		if err != nil {
			t.Fatalf("CredentialedProviders: %v", err)
		}
		// Sorted in SupportedProviders order regardless of which source (store
		// vs env) supplied each one.
		want := []string{"anthropic", "openai"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CredentialedProviders = %v, want %v", got, want)
		}
	})
}

// TestTranscript proves the read-only transcript path needs no credential:
// it drives a real turn through the injected-provider test seam, closes the
// runner, then reads it back via Transcript with BOTH provider API keys
// explicitly unset — a live credential is never consulted.
func TestTranscript(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	prov := &scriptedProvider{events: [][]provider.StreamEvent{
		{
			{Type: provider.StreamTextDelta, Text: "hi there"},
			{Type: provider.StreamFinished, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 3, OutputTokens: 2}},
		},
	}}

	r, err := runner.NewSession(context.Background(), runner.Options{
		Root: root, Cwd: cwd, Model: testModel, System: "test system",
		Provider: prov,
		Tools:    oneToolRegistry{},
		IDGen:    seqIDGen(),
		Clock:    seqClock(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := r.ID()

	if err := r.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// No credential needed to read the transcript back.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	msgs, err := runner.Transcript(context.Background(), id, root)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Transcript: got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != provider.RoleUser || msgText(msgs[0]) != "hello" {
		t.Errorf("msgs[0] = %+v, want user %q", msgs[0], "hello")
	}
	if msgs[1].Role != provider.RoleAssistant || msgText(msgs[1]) != "hi there" {
		t.Errorf("msgs[1] = %+v, want assistant %q", msgs[1], "hi there")
	}
}

// TestTranscript_UnknownSession asserts a not-found session surfaces a
// legible, wrapped error rather than a bare SDK sentinel.
func TestTranscript_UnknownSession(t *testing.T) {
	root := t.TempDir()
	_, err := runner.Transcript(context.Background(), "does-not-exist", root)
	if err == nil {
		t.Fatal("Transcript: got nil error, want one for an unknown session id")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("Transcript error = %v, want it to wrap session.ErrSessionNotFound", err)
	}
}

// TestProviderTablesInSync enforces the invariant that every provider in
// SupportedProviders has both a default model and an API-key env-var mapping.
// Without it, adding a provider to supportedProviders but forgetting it in
// defaultModels (models.go) or envVars (provider.go) would silently resolve to
// an empty model ("runner: unknown model \"\"") or an unreachable env fallback.
// This fails loudly at CI time instead.
func TestProviderTablesInSync(t *testing.T) {
	for _, p := range runner.SupportedProviders() {
		if runner.DefaultModel(p) == "" {
			t.Errorf("provider %q is in SupportedProviders but missing from defaultModels", p)
		}
		if runner.EnvVar(p) == "" {
			t.Errorf("provider %q is in SupportedProviders but missing from envVars", p)
		}
	}
}
