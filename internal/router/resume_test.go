package router

// resume_test.go covers issue #139: resuming a session with NO live worker
// spawns a fresh one and rebuilds it from the on-disk journal — WITHOUT
// re-delivering the journal replay to already-attached clients through the event
// sink (the constraint the whole slice turns on). It drives the same real,
// killable faux workers TestCrashIsolation uses.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// recordingRelay is a [github.com/jedwards1230/gofer/internal/daemon.EventRelay]
// stand-in that records every broadcast the router's event sink drives through
// it. Recording it directly is enough to prove the resume-replay suppression
// without standing up a full daemon: the sink calls exactly these two methods.
type recordingRelay struct {
	mu      sync.Mutex
	raw     []json.RawMessage
	updates int
}

func (r *recordingRelay) BroadcastRawEvent(_ string, raw json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make(json.RawMessage, len(raw))
	copy(cp, raw)
	r.raw = append(r.raw, cp)
}

func (r *recordingRelay) BroadcastSessionUpdate(string, event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates++
}

func (r *recordingRelay) rawCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.raw)
}

// newResumeSupervisor builds a router whose workers are the re-exec faux workers
// (with ResumeSession wired for the offline-resume path), and reaps them on
// cleanup so no detached worker leaks past the test binary.
func newResumeSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	shortRuntimeDir(t)
	root := t.TempDir()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})
	return sup
}

// makeOfflineSession creates a session, runs one turn so it journals a real
// transcript, then crashes its worker so the session is OFFLINE with a durable
// journal — the exact precondition offline resume exists for. It returns the id
// and the folded history captured before the crash.
func makeOfflineSession(t *testing.T, sup *Supervisor, dir string) (string, []provider.Message) {
	t.Helper()
	ctx := context.Background()

	info, err := sup.Create(ctx, "hello", supervisor.CreateOptions{Cwd: dir})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := info.ID

	// Let the turn finish and journal before we read/crash (issue #137).
	settleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_ = sup.AwaitSettled(settleCtx, id)
	cancel()
	before := waitHistoryAtLeast(t, sup, id, 1)

	crashWorker(t, sup, id)
	waitOffline(t, sup, id)
	return id, before
}

// waitHistoryAtLeast polls History until it folds at least minLen messages, so a
// test never races the worker's asynchronous journaling.
func waitHistoryAtLeast(t *testing.T, sup *Supervisor, id string, minLen int) []provider.Message {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		h, err := sup.History(context.Background(), id)
		if err == nil && len(h) >= minLen {
			return h
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("history for %s never folded %d messages", id, minLen)
	return nil
}

// TestResumeOfflineSpawnsFreshWorker is the shippable value of #139: resuming a
// session with no live worker spawns a fresh one and rebuilds it from the
// journal, so the session becomes live with its full transcript intact.
func TestResumeOfflineSpawnsFreshWorker(t *testing.T) {
	sup := newResumeSupervisor(t)
	ctx := context.Background()
	dir := t.TempDir()

	id, before := makeOfflineSession(t, sup, dir)
	if _, ok := sup.get(id); ok {
		t.Fatal("session still live after crash; cannot exercise the offline path")
	}

	info, err := sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: dir})
	if err != nil {
		t.Fatalf("resume offline session: %v", err)
	}
	if !info.Live {
		t.Errorf("resumed session snapshot Live=false, want true")
	}
	if _, ok := sup.get(id); !ok {
		t.Errorf("resume did not register a live worker handle for %s", id)
	}

	// Rebuilt from the journal: the transcript matches what was durable before the
	// crash (leverages #137's complete-history read).
	after := waitHistoryAtLeast(t, sup, id, len(before))
	if len(after) != len(before) {
		t.Errorf("history after resume = %d messages, want %d (rebuild lost or duplicated entries)", len(after), len(before))
	}
}

// TestResumeOfflineDoesNotDoubleBroadcast is the constraint the slice turns on:
// the journal replay a fresh worker performs on resume must NOT flow through the
// event sink to already-attached clients (the daemon's own History replay is the
// single client-facing one). After resume, a genuinely live turn must broadcast
// normally, proving the suppression is scoped to the replay and then lifted.
//
// Mutation check: deleting the `replaySuppressed` guard in eventSink (or the
// Store(true) in resumeOffline) makes the mid-test count assertion below fire —
// the replayed history frames reach the recording relay. Verified RED by hand.
func TestResumeOfflineDoesNotDoubleBroadcast(t *testing.T) {
	sup := newResumeSupervisor(t)
	ctx := context.Background()
	dir := t.TempDir()

	id, before := makeOfflineSession(t, sup, dir)
	if len(before) == 0 {
		t.Fatal("offline session has no history — nothing could be double-broadcast")
	}

	// Install the relay only NOW, after the create turn and crash, so it observes
	// exactly what resume and the subsequent live turn broadcast — nothing else.
	relay := &recordingRelay{}
	sup.SetEventRelay(relay)

	info, err := sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: dir})
	if err != nil {
		t.Fatalf("resume offline session: %v", err)
	}
	if !info.Live {
		t.Fatalf("resumed session snapshot Live=false, want true")
	}

	// By the time Resume returns, rec.Load has drained and applied every replayed
	// frame through the (suppressed) sink and the guard is cleared — so this count
	// is authoritative, not racy. Zero broadcasts means the replay stayed silent.
	if n := relay.rawCount(); n != 0 {
		t.Fatalf("resume replay leaked %d gofer/event frames through the sink; want 0 (they would double the transcript for an attached client)", n)
	}
	if relay.updates != 0 {
		t.Fatalf("resume replay leaked %d session/update projections through the sink; want 0", relay.updates)
	}

	// A fresh live turn AFTER resume must broadcast normally — the guard is scoped
	// to the replay, not the handle.
	if err := sup.Send(ctx, id, "again"); err != nil {
		t.Fatalf("send after resume: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for relay.rawCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if relay.rawCount() == 0 {
		t.Errorf("a live turn after resume broadcast nothing through the sink; suppression was not lifted")
	}
}

// TestResumeUnknownIDReturnsNotFound: an id with neither a live worker nor a
// journal is a genuine not-found, not a spawn-over-nothing.
func TestResumeUnknownIDReturnsNotFound(t *testing.T) {
	sup := newResumeSupervisor(t)
	unknown := uuid.Must(uuid.NewV7()).String()

	_, err := sup.Resume(context.Background(), unknown, supervisor.ResumeOptions{Cwd: t.TempDir()})
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resume of unknown id = %v, want an error wrapping session.ErrSessionNotFound", err)
	}
}

// TestResumeLiveSessionAttachesWithoutRespawn guards the existing live-attach
// path (design §7): resuming a session this router already hosts returns the live
// snapshot and reuses the SAME worker handle — it never forks a second worker.
func TestResumeLiveSessionAttachesWithoutRespawn(t *testing.T) {
	sup := newResumeSupervisor(t)
	ctx := context.Background()
	dir := t.TempDir()

	info, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: dir})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h1, ok := sup.get(info.ID)
	if !ok {
		t.Fatalf("created session %s has no live handle", info.ID)
	}

	got, err := sup.Resume(ctx, info.ID, supervisor.ResumeOptions{Cwd: dir})
	if err != nil {
		t.Fatalf("resume live session: %v", err)
	}
	if !got.Live {
		t.Errorf("resume of a live session returned Live=false")
	}
	h2, ok := sup.get(info.ID)
	if !ok || h1 != h2 {
		t.Errorf("resume of a LIVE session replaced its worker handle; want the same handle (no re-spawn)")
	}
}
