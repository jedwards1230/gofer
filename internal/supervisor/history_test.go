package supervisor_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSupervisor_History asserts History returns the live session's folded
// conversation history unchanged.
func TestSupervisor_History(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hello"}}},
	}
	h.session(entry.ID).setFold(want)

	got, err := h.sup.History(ctx, entry.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("History = %+v, want %+v", got, want)
	}
}

// TestSupervisor_History_NotLive asserts History on an unknown/not-live id
// surfaces [supervisor.ErrNotLive] rather than a zero-value success.
func TestSupervisor_History_NotLive(t *testing.T) {
	h := newHarness(t)

	if _, err := h.sup.History(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("History for unknown session: want an error, got none")
	}
}

// TestSupervisor_SessionInfo_Cwd asserts a created and a resumed session's
// SessionInfo.Cwd reflects the cwd passed to Create/Resume.
func TestSupervisor_SessionInfo_Cwd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj/created", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Cwd != "/proj/created" {
		t.Errorf("created.Cwd = %q, want /proj/created", created.Cwd)
	}

	// Kill it so Resume below builds a fresh managed session (Resume is a
	// no-op returning the existing snapshot for an already-live id).
	if err := h.sup.Kill(ctx, created.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	resumed, err := h.sup.Resume(ctx, created.ID, supervisor.ResumeOptions{Cwd: "/proj/resumed", Model: "m"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Cwd != "/proj/resumed" {
		t.Errorf("resumed.Cwd = %q, want /proj/resumed", resumed.Cwd)
	}
}
