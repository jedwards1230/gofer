package tuibridge_test

// resume_test.go covers the in-process half of the /resume plumbing: the
// Adapter's ListSessions (the supervisor's store-wide enumeration mapped to the
// picker's row) and Resume (which must resolve a model per call, since the
// supervisor requires one on every Resume and the journal does not persist it).

import (
	"context"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tuibridge"
)

// newResumeRecordingSupervisor builds a Supervisor that records the model each
// RESUME reaches the runner with (the create path is recorded too, so a test
// can tell the two apart by call order), substituting the faux provider so no
// network is touched.
func newResumeRecordingSupervisor(t *testing.T, resumedModels *[]string) *supervisor.Supervisor {
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
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(faux.Default())
			return runner.New(ctx, opts)
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			*resumedModels = append(*resumedModels, opts.Model)
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(faux.Default())
			return runner.Resume(ctx, id, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// TestAdapterListSessionsMapsDiskRows proves the store-wide enumeration reaches
// the picker's row type with the fields it renders — including for a session
// that is no longer live, which is the whole case /resume exists for.
func TestAdapterListSessionsMapsDiskRows(t *testing.T) {
	var resumed []string
	sup := newResumeRecordingSupervisor(t, &resumed)
	a := tuibridge.New(sup, func(context.Context) string { return "claude-sonnet-5" })

	cwd := t.TempDir()
	info, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := a.Archive(context.Background(), info.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	refs, err := a.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var found bool
	for _, r := range refs {
		if r.ID != info.ID {
			continue
		}
		found = true
		if r.Cwd != cwd {
			t.Errorf("Cwd = %q, want %q", r.Cwd, cwd)
		}
	}
	if !found {
		t.Fatalf("archived session %s missing from ListSessions: %+v", info.ID, refs)
	}
}

// TestAdapterResumeResolvesModelPerCall proves Resume threads a freshly
// resolved model down to runner.Resume. The supervisor requires one on every
// Resume ([supervisor.ResumeOptions]: "Model and Cwd are required — the journal
// itself does not persist them"), so passing the empty string would fail the
// reopen at the SDK rather than here — and reading a resolver rather than a
// value captured at construction is what lets a `/model` write made since apply
// (the same rule Create follows, issue #156).
func TestAdapterResumeResolvesModelPerCall(t *testing.T) {
	var resumed []string
	sup := newResumeRecordingSupervisor(t, &resumed)

	current := "claude-sonnet-5"
	a := tuibridge.New(sup, func(context.Context) string { return current })

	cwd := t.TempDir()
	info, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := a.Archive(context.Background(), info.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	current = "claude-opus-4-8"
	if err := a.Resume(context.Background(), info.ID, cwd); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if len(resumed) != 1 {
		t.Fatalf("ResumeSession called %d times, want 1", len(resumed))
	}
	if resumed[0] != "claude-opus-4-8" {
		t.Errorf("resume model = %q, want the CURRENT default claude-opus-4-8 — the resolver is being read once at construction, not per call", resumed[0])
	}
}

// TestAdapterResumeIsIdempotentForLiveSession pins the contract the TUI's
// already-live shortcut leans on: resuming a session that is already under
// supervision succeeds and builds no second runner.
func TestAdapterResumeIsIdempotentForLiveSession(t *testing.T) {
	var resumed []string
	sup := newResumeRecordingSupervisor(t, &resumed)
	a := tuibridge.New(sup, func(context.Context) string { return "claude-sonnet-5" })

	info, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := a.Resume(context.Background(), info.ID, t.TempDir()); err != nil {
		t.Fatalf("Resume of a live session: %v", err)
	}
	if len(resumed) != 0 {
		t.Errorf("ResumeSession called %d times for an already-live session, want 0", len(resumed))
	}
}
