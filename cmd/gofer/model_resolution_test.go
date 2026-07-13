package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/runner"
)

// wantNoProviderCredentialsMsg is the exact neutral (no-flagship-vendor)
// message resolveRunModel returns when no provider has any credential
// configured.
const wantNoProviderCredentialsMsg = "no provider credentials — run 'gofer login anthropic' or 'gofer login openai' (or set ANTHROPIC_API_KEY / OPENAI_API_KEY)"

// wantAmbiguousModelMsg is the exact usage-error message resolveRunModel
// returns when more than one provider has a credential configured and -m
// was not given.
const wantAmbiguousModelMsg = "multiple providers have credentials — pass -m (e.g. -m claude-sonnet-5 or -m gpt-5)"

// TestErrNoProviderCredentials_Message locks the exact wording of the
// no-credentials error — gofer's messaging never names a default vendor.
func TestErrNoProviderCredentials_Message(t *testing.T) {
	if got := errNoProviderCredentials.Error(); got != wantNoProviderCredentialsMsg {
		t.Errorf("errNoProviderCredentials.Error() = %q, want %q", got, wantNoProviderCredentialsMsg)
	}
}

// TestResolveRunModel covers the none/one/many resolution outcomes directly
// (not through run(), which for the one-credential case would go on to
// attempt a live Stream call) — hermetic via t.Setenv on both provider
// env vars and a fresh --root.
func TestResolveRunModel(t *testing.T) {
	t.Run("no credentials", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "")

		_, err := resolveRunModel(context.Background(), root)
		if err == nil {
			t.Fatal("resolveRunModel: got nil error, want errNoProviderCredentials")
		}
		if !errors.Is(err, errNoProviderCredentials) {
			t.Errorf("resolveRunModel err = %v, want errNoProviderCredentials", err)
		}
		if err.Error() != wantNoProviderCredentialsMsg {
			t.Errorf("resolveRunModel err = %q, want %q", err.Error(), wantNoProviderCredentialsMsg)
		}
	})

	t.Run("one credential", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
		t.Setenv("OPENAI_API_KEY", "")

		model, err := resolveRunModel(context.Background(), root)
		if err != nil {
			t.Fatalf("resolveRunModel: %v", err)
		}
		if model != "claude-sonnet-5" {
			t.Errorf("resolveRunModel model = %q, want claude-sonnet-5", model)
		}
	})

	t.Run("multiple credentials", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
		t.Setenv("OPENAI_API_KEY", "sk-test-key")

		_, err := resolveRunModel(context.Background(), root)
		if err == nil {
			t.Fatal("resolveRunModel: got nil error, want an ambiguous-model usage error")
		}
		var uerr *usageError
		if !errors.As(err, &uerr) {
			t.Fatalf("resolveRunModel err = %T (%v), want *usageError", err, err)
		}
		if got := err.Error(); got != wantAmbiguousModelMsg {
			t.Errorf("resolveRunModel err = %q, want %q", got, wantAmbiguousModelMsg)
		}
	})
}

// TestRun_NoProviderCredentials drives the full dispatch: `gofer run` with no
// -m and no credentials anywhere fails fast (before any prompt read) with
// exit 1 and the exact neutral message on stderr.
//
// hermeticDaemonEnv is required here: run() dials for a daemon before this
// failure is even reached (see runRun's doc), and this test passes no
// --daemon flag — without it, a real `gofer daemon` running on the host
// would get discovered and this assertion would break (see
// hermeticDaemonEnv's doc).
func TestRun_NoProviderCredentials(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	var out, errBuf bytes.Buffer
	got := run([]string{"run", "--root", root}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run() = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), wantNoProviderCredentialsMsg) {
		t.Errorf("stderr = %q, want it to contain %q", errBuf.String(), wantNoProviderCredentialsMsg)
	}
}

// TestRun_AmbiguousProviderCredentials drives the full dispatch: `gofer run`
// with no -m and BOTH providers credentialed is a usage error (exit 2) —
// gofer picks no favorite among logged-in providers.
//
// hermeticDaemonEnv is required for the same reason as
// TestRun_NoProviderCredentials — see its doc.
func TestRun_AmbiguousProviderCredentials(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("OPENAI_API_KEY", "sk-test-key")

	var out, errBuf bytes.Buffer
	got := run([]string{"run", "--root", root, "do x"}, strings.NewReader(""), &out, &errBuf)
	if got != 2 {
		t.Fatalf("run() = %d, want 2\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), wantAmbiguousModelMsg) {
		t.Errorf("stderr = %q, want it to contain %q", errBuf.String(), wantAmbiguousModelMsg)
	}
}

// TestWrapCredentialHint locks the exact wording gofer adds back onto the
// SDK's app-neutral [runner.NoCredentialError] — this is the message the
// now-deleted internal/runner package used to produce directly — and that
// errors.Is against runner.ErrNoCredential still holds through the wrap.
// Any other error (including one that isn't a credential error at all) must
// pass through unchanged.
func TestWrapCredentialHint(t *testing.T) {
	t.Run("with env var", func(t *testing.T) {
		orig := &runner.NoCredentialError{Provider: "anthropic", EnvVar: "ANTHROPIC_API_KEY"}
		got := wrapCredentialHint(orig)
		const want = "no credential for anthropic — run 'gofer login anthropic' or set ANTHROPIC_API_KEY"
		if got.Error() != want {
			t.Errorf("wrapCredentialHint().Error() = %q, want %q", got.Error(), want)
		}
		if !errors.Is(got, runner.ErrNoCredential) {
			t.Error("wrapCredentialHint result does not satisfy errors.Is(_, runner.ErrNoCredential)")
		}
	})

	t.Run("no env var known", func(t *testing.T) {
		orig := &runner.NoCredentialError{Provider: "widget"}
		got := wrapCredentialHint(orig)
		const want = "no credential for widget — run 'gofer login widget'"
		if got.Error() != want {
			t.Errorf("wrapCredentialHint().Error() = %q, want %q", got.Error(), want)
		}
	})

	t.Run("non-credential error passes through unchanged", func(t *testing.T) {
		orig := errors.New("boom")
		if got := wrapCredentialHint(orig); got != orig {
			t.Errorf("wrapCredentialHint(%v) = %v, want the same error unchanged", orig, got)
		}
	})

	t.Run("nil passes through", func(t *testing.T) {
		if got := wrapCredentialHint(nil); got != nil {
			t.Errorf("wrapCredentialHint(nil) = %v, want nil", got)
		}
	})
}

// TestRun_NoCredentialForModel drives the full dispatch: `gofer run -m
// <model>` for a provider with no stored login and no env var configured
// fails with gofer's own 'gofer login' hint, not the SDK's bare app-neutral
// message — the credential error now originates in agent-sdk-go/runner
// (deliberately app-neutral there), and cmd/gofer is the one place that adds
// the CLI-specific remediation back (see wrapCredentialHint).
//
// hermeticDaemonEnv is required for the same reason as
// TestRun_NoProviderCredentials — see its doc.
func TestRun_NoCredentialForModel(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	var out, errBuf bytes.Buffer
	got := run([]string{"run", "-m", "claude-sonnet-5", "--root", root, "do a thing"}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run() = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	const want = "no credential for anthropic — run 'gofer login anthropic' or set ANTHROPIC_API_KEY"
	if !strings.Contains(errBuf.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", errBuf.String(), want)
	}
}

// TestLogin_NoArgLists drives the full dispatch: `gofer login` with no
// provider argument exits 0 and lists every supported provider on stdout.
func TestLogin_NoArgLists(t *testing.T) {
	var out, errBuf bytes.Buffer
	got := run([]string{"login"}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run() = %d, want 0\nstderr: %s", got, errBuf.String())
	}
	for _, want := range []string{"anthropic", "openai"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout = %q, want it to list %q", out.String(), want)
		}
	}
}
