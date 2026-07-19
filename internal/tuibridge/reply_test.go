package tuibridge_test

// reply_test.go covers Adapter.Reply's verdict mapping against a real
// *supervisor.Supervisor — the M3 approvals-relay contract #3 client side
// (Worker C's tuibridge half; internal/daemonbridge's own
// TestPermissionRelayEndToEnd is the other transport's live proof of the
// same underlying supervisor.Reply → loop.Gate.Reply path, exercised through
// a real Guard/Ask/Approver turn). Adapter.Reply's own logic is a trivial
// allow/deny → event.Verdict mapping plus a passthrough call, so this test's
// job is narrower: prove it reaches a live session's gate without error for
// both verdicts, against a real (if tool-call-free) supervisor session —
// not to re-litigate the SDK's own gate semantics.

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

// newTestSupervisor builds a Supervisor whose sessions are real
// [runner.Runner]s over a faux, no-network provider — mirroring
// internal/daemonbridge's own bridge_test.go helper of the same name
// (unexported to that package, so redeclared here).
func newTestSupervisor(t *testing.T) *supervisor.Supervisor {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(faux.Default())
			return runner.New(ctx, opts)
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// TestAdapterReplyReachesSessionGate asserts Reply(allow=true) and
// Reply(allow=false) both route to a live session without error — the
// Adapter's whole responsibility, since the verdict/remember mapping itself
// has no branch left to get wrong (see event.PermissionReply's own
// omitempty-mirrored shape).
func TestAdapterReplyReachesSessionGate(t *testing.T) {
	sup := newTestSupervisor(t)
	a := tuibridge.New(sup, fixedModel("faux"))

	info, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := a.Reply(context.Background(), info.ID, "perm-1", true, true); err != nil {
		t.Errorf("Reply(allow=true, remember=true): %v", err)
	}
	if err := a.Reply(context.Background(), info.ID, "perm-2", false, false); err != nil {
		t.Errorf("Reply(allow=false, remember=false): %v", err)
	}
}

// TestAdapterReplyUnknownSessionErrors asserts Reply surfaces the
// supervisor's own "no such session" error rather than swallowing it —
// matching every other Adapter method's error passthrough.
func TestAdapterReplyUnknownSessionErrors(t *testing.T) {
	sup := newTestSupervisor(t)
	a := tuibridge.New(sup, fixedModel("faux"))

	if err := a.Reply(context.Background(), "no-such-session", "perm-1", true, false); err == nil {
		t.Error("Reply for an unknown session: want an error, got nil")
	}
}
