package tuibridge_test

// defaultmodel_test.go covers the Adapter's create-time model fallback — the
// local-TUI half of issue #147. The roster TUI resolves a model for its header
// but supplies none on the CreateOptions it hands the bridge, so before this
// fallback existed the supervisor received an empty model id and the session
// died on `runner: unknown model ""` while the header displayed a real model.
// The daemon path never had this bug: it resolves session/new against
// daemon.Config.DefaultModel, which is the shape mirrored here.

import (
	"context"
	"errors"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tuibridge"
)

// fixedModel is the trivial resolver: a default that genuinely never changes,
// for the tests below whose subject is the fallback ORDER rather than the
// fallback's freshness. Tests about freshness (see
// TestAdapterCreateReresolvesDefaultPerCreate) supply a resolver that changes.
func fixedModel(id string) func(context.Context) string {
	return func(context.Context) string { return id }
}

// newModelRecordingSupervisor builds a Supervisor that records the model each
// Create actually reaches the runner with, then substitutes the faux provider
// so no network is touched. It deliberately does NOT reuse
// newTestSupervisor: that helper overwrites opts.Model unconditionally, which
// would mask the very value under test.
func newModelRecordingSupervisor(t *testing.T, seen *[]string) *supervisor.Supervisor {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			*seen = append(*seen, opts.Model)
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(faux.Default())
			return runner.New(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// TestAdapterCreateUsesDefaultModel proves an unset CreateOptions.Model
// resolves to the adapter's default all the way down to the runner options —
// not merely that Create returns without error, which it did before the fix
// too (the empty model only failed later, inside the SDK).
func TestAdapterCreateUsesDefaultModel(t *testing.T) {
	var seen []string
	a := tuibridge.New(newModelRecordingSupervisor(t, &seen), fixedModel("claude-sonnet-5"))

	if _, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("NewSession called %d times, want 1", len(seen))
	}
	if seen[0] != "claude-sonnet-5" {
		t.Errorf("runner options Model = %q, want the adapter default claude-sonnet-5", seen[0])
	}
}

// TestAdapterCreateExplicitModelWins proves the fallback is exactly that: a
// caller-supplied model is passed through untouched, so a per-session model
// override is never silently replaced by the adapter's default.
func TestAdapterCreateExplicitModelWins(t *testing.T) {
	var seen []string
	a := tuibridge.New(newModelRecordingSupervisor(t, &seen), fixedModel("claude-sonnet-5"))

	opts := tui.CreateOptions{Cwd: t.TempDir(), Model: "gpt-5"}
	if _, err := a.Create(context.Background(), "", opts); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("NewSession called %d times, want 1", len(seen))
	}
	if seen[0] != "gpt-5" {
		t.Errorf("runner options Model = %q, want the explicit gpt-5", seen[0])
	}
}

// TestAdapterCreateNoModelAnywhereIsActionable covers the remaining gap: an
// adapter with no default AND a caller with no model must fail with
// supervisor.ErrNoModel, whose message names the remedy — not with the SDK's
// `runner: unknown model ""`, which named neither the cause nor a fix and was
// the message users actually saw (issue #147).
func TestAdapterCreateNoModelAnywhereIsActionable(t *testing.T) {
	var seen []string
	a := tuibridge.New(newModelRecordingSupervisor(t, &seen), fixedModel(""))

	_, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("Create with no model anywhere: got nil error, want supervisor.ErrNoModel")
	}
	if !errors.Is(err, supervisor.ErrNoModel) {
		t.Errorf("Create err = %v, want supervisor.ErrNoModel", err)
	}
	if len(seen) != 0 {
		t.Errorf("NewSession ran %d times with an empty model, want 0 — the guard must fire first", len(seen))
	}
}

// TestAdapterCreateReresolvesDefaultPerCreate is the regression guard for
// issue #156's TUI half. The adapter used to capture the default model BY
// VALUE at construction, so the fallback was frozen for the process's whole
// life: `/model` wrote a new session.model into config.json, the status line
// said the default was set, and every session created afterwards still ran the
// model this process had started with. No amount of reselecting — or even
// restarting the TUI client, on the daemon path — could reach it.
//
// The proof has to be that a SECOND create sees a CHANGED default, not merely
// that one create sees the right value: a captured string passes the
// single-create version of this test perfectly.
//
// nil, meanwhile, must stay survivable rather than panicking — the adapter's
// contract is that a missing default fails with the actionable
// supervisor.ErrNoModel, not with a nil-func dereference.
func TestAdapterCreateReresolvesDefaultPerCreate(t *testing.T) {
	var seen []string
	current := "claude-sonnet-5"
	a := tuibridge.New(newModelRecordingSupervisor(t, &seen), func(context.Context) string { return current })

	if _, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Stand-in for what /model does: change the one source of truth the
	// resolver reads (config.json in the real wiring, this variable here).
	current = "claude-haiku-4-5"

	if _, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("second Create: %v", err)
	}

	want := []string{"claude-sonnet-5", "claude-haiku-4-5"}
	if len(seen) != len(want) {
		t.Fatalf("NewSession models = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("NewSession models = %v, want %v — a create after the default changed must use the NEW default, not the one captured at construction", seen, want)
		}
	}
}

// TestAdapterNilResolverIsNotAPanic pins the nil-resolver contract the doc
// promises: no default is a normal state (no credential yet), and it must
// surface as supervisor.ErrNoModel rather than as a nil-func call.
func TestAdapterNilResolverIsNotAPanic(t *testing.T) {
	var seen []string
	a := tuibridge.New(newModelRecordingSupervisor(t, &seen), nil)

	_, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if !errors.Is(err, supervisor.ErrNoModel) {
		t.Errorf("Create with a nil resolver err = %v, want supervisor.ErrNoModel", err)
	}
}
