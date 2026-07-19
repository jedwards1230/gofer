package main

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestFlagWasSet covers the provenance predicate the pinning rule hangs on: it
// must report whether the operator PASSED a flag, not whether the flag holds a
// non-zero value. An explicitly passed value that happens to equal the default
// is still explicitly passed.
func TestFlagWasSet(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"not passed at all", nil, false},
		{"other flags passed", []string{"--listen", "127.0.0.1:65535"}, false},
		{"passed with a value", []string{"--model", "claude-haiku-4-5"}, true},
		{"passed with an empty value", []string{"--model", ""}, true},
		{"passed equal to the flag's own default", []string{"--model", "the-default"}, true},
		{"passed with =", []string{"--model=claude-haiku-4-5"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
			fs.SetOutput(nopWriter{})
			fs.String("model", "the-default", "")
			fs.String("listen", "", "")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("parse %v: %v", tt.args, err)
			}
			if got := flagWasSet(fs, "model"); got != tt.want {
				t.Errorf("flagWasSet(--model) with args %v = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// nopWriter discards flag-package output.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestModelFlagPinned covers the full pinning rule as the serve path applies
// it: BOTH that the operator passed --model and that the value is non-empty.
// `--model ""` is explicitly "no pinned model" — startup falls through to
// resolveRunModel for it exactly as it does for an omitted flag, so treating it
// as a pin would freeze the daemon on a value nobody chose.
func TestModelFlagPinned(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"omitted: config may retarget", nil, false},
		{"explicit non-empty: pinned", []string{"--model", "claude-haiku-4-5"}, true},
		{"explicit empty: NOT pinned", []string{"--model", ""}, false},
		{"unrelated flag only", []string{"--listen", "127.0.0.1:65535"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
			fs.SetOutput(nopWriter{})
			model := fs.String("model", "", "")
			fs.String("listen", "", "")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("parse %v: %v", tt.args, err)
			}
			if got := modelFlagPinned(fs, *model); got != tt.want {
				t.Errorf("modelFlagPinned with args %v = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestModelFlagPinnedIsProvenanceNotValue backs the claim modelFlagPinned's doc
// makes — that it asks "did the operator pass --model", not "is the value
// non-empty".
//
// It uses a NON-EMPTY flag default on purpose, because that is the only
// configuration in which the two rules differ. With today's empty default they
// are behaviourally identical, so a value-only rule would pass every other test
// in this file; this is what stops the provenance half from silently rotting
// into a no-op if the flag ever gains a default (which is exactly when treating
// an unpassed flag as a pin would start freezing daemons nobody pinned).
func TestModelFlagPinnedIsProvenanceNotValue(t *testing.T) {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	model := fs.String("model", "some-future-default", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *model == "" {
		t.Fatal("test setup: the flag default must be non-empty for this case to mean anything")
	}
	if modelFlagPinned(fs, *model) {
		t.Error("an unpassed --model holding a non-empty DEFAULT was treated as pinned; pinning must follow provenance (was the flag passed), not the value")
	}
}

// TestDaemonDefaultModelResolverPinnedIsNil asserts the pinning decision: a
// daemon started with an explicit --model gets a NIL resolver, which is what
// makes its flag authoritative for the process lifetime. A config write by any
// attached client must not be able to retarget it, or --model would be advisory
// rather than a pin (issue #147's whole reason for existing).
func TestDaemonDefaultModelResolverPinnedIsNil(t *testing.T) {
	if got := daemonDefaultModelResolver(true, t.TempDir()); got != nil {
		t.Error("daemonDefaultModelResolver(pinned) returned a resolver; an explicitly flagged daemon must never be retargeted by a config write")
	}
}

// TestDaemonDefaultModelResolverObservesConfigWrites is the end-to-end half of
// the #156 fix on the cmd/gofer side, against a REAL config.json on a temp root:
// the resolver an unpinned daemon installs must return whatever `session.model`
// currently says, including after the file is rewritten — which is exactly what
// `/model` does.
func TestDaemonDefaultModelResolverObservesConfigWrites(t *testing.T) {
	root := t.TempDir()
	writeSessionModel(t, root, "claude-sonnet-4-5")

	resolve := daemonDefaultModelResolver(false, root)
	if resolve == nil {
		t.Fatal("daemonDefaultModelResolver(unpinned) = nil, want a resolver")
	}

	got, err := resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "claude-sonnet-4-5" {
		t.Fatalf("resolve = %q, want the configured model", got)
	}

	// The operator runs /model. Nothing restarts.
	writeSessionModel(t, root, "claude-haiku-4-5")

	got, err = resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve after rewrite: %v", err)
	}
	if got != "claude-haiku-4-5" {
		t.Errorf("resolve after rewrite = %q, want the rewritten model — the daemon would still be frozen (issue #156)", got)
	}
}

// TestDaemonDefaultModelResolverSurfacesAMalformedConfig asserts the resolver
// reports a malformed config as an ERROR rather than silently answering "".
// The non-fatal degradation is the daemon's job (Daemon.defaultModel keeps the
// startup value and logs); swallowing it here would deny it the chance to log,
// and would be indistinguishable from a config with no model set.
func TestDaemonDefaultModelResolverSurfacesAMalformedConfig(t *testing.T) {
	root := t.TempDir()
	if err := writeRawConfig(root, "{not json"); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}

	resolve := daemonDefaultModelResolver(false, root)
	got, err := resolve(context.Background())
	if err == nil {
		t.Fatalf("resolve on a malformed config = (%q, nil), want an error", got)
	}
	if got != "" {
		t.Errorf("resolve on a malformed config returned model %q alongside the error, want empty", got)
	}
}

// writeSessionModel writes a config.json at root whose session.model is model.
func writeSessionModel(t *testing.T, root, model string) {
	t.Helper()
	var cfg config.Config
	cfg.Session.Model = model
	if err := config.Save(config.DefaultPath(root), cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// writeRawConfig writes arbitrary bytes as root's config.json, for the
// malformed-file case config.Save could not produce.
func writeRawConfig(root, body string) error {
	return os.WriteFile(config.DefaultPath(root), []byte(body), 0o600)
}

// TestSessionModelRoundTripsThroughConfig guards the assumption the resolver
// rests on: config.Save/Load actually round-trip session.model, so a green
// resolver test is not green merely because both reads returned the zero value.
func TestSessionModelRoundTripsThroughConfig(t *testing.T) {
	root := t.TempDir()
	writeSessionModel(t, root, "claude-opus-4-1")
	cfg, err := config.Load(config.DefaultPath(root))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Session.Model != "claude-opus-4-1" {
		t.Fatalf("round-tripped session.model = %q, want %q", cfg.Session.Model, "claude-opus-4-1")
	}
	if !strings.Contains(config.DefaultPath(root), "config.json") {
		t.Errorf("DefaultPath = %q, want it to name config.json", config.DefaultPath(root))
	}
}
