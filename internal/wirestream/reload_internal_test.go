package wirestream

// reload_internal_test.go pins WHICH references may consume a session's pending
// cwd-missing retry ([sessionState.reload], armed by loadHistory when the daemon
// refuses a load because the session's recorded directory is gone —
// jedwards1230/gofer#326).
//
// The distinction is not cosmetic. Consuming an arming issues a REAL
// session/load, so a lookup that consumes one turns an ordinary bookkeeping call
// into an RPC. [Reconstructor.session] is called for every inbound event frame
// (handleGoferEvent) and for every turn end, on the demuxer goroutine — a retry
// fired from there lands in the middle of another load's replay and duplicates
// the session's entire history onto the broker. Only the three consumer-facing
// entries ([Reconstructor.reference]: Subscribe, SubscribeLive, Load) mean "a
// consumer is attaching", which is the only reference kind a retry may answer.
//
// These are internal tests over the bare-Reconstructor seam the other
// *_internal_test.go files use: no *daemon.Client, so they assert the flag the
// decision is made on rather than the RPC it would produce (with a nil client an
// actually-issued load would panic on another goroutine, which is a crash rather
// than a failed assertion).

import (
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// armedSession returns a Reconstructor holding one session whose history load
// has failed with the cwd-missing signal — i.e. with a retry armed and waiting
// for the next consumer reference.
func armedSession(t *testing.T, id string) (*Reconstructor, *sessionState) {
	t.Helper()
	r := newReconstructTestReconstructor()
	rec := newSessionState(id)
	rec.settleLoad() // the failed load settled, exactly as finishLoad leaves it
	r.sessions[id] = rec
	r.armReload(rec)
	if !rec.reload {
		t.Fatal("armReload did not arm the retry; this test would prove nothing")
	}
	return r, rec
}

// TestSessionLookupDoesNotConsumeTheCwdMissingRetry is the invariant that keeps
// the retry off the demuxer's hot path: a bare session() lookup — what
// handleGoferEvent and handleTurnEnd do, per inbound frame — must leave the
// arming untouched for the consumer that actually attaches.
func TestSessionLookupDoesNotConsumeTheCwdMissingRetry(t *testing.T) {
	const sid = "sess-1"
	r, rec := armedSession(t, sid)

	if got := r.session(sid); got != rec {
		t.Fatalf("session(%q) returned a different entry", sid)
	}
	if !rec.reload {
		t.Error("a bare session() lookup consumed the cwd-missing retry. That lookup runs per inbound event " +
			"frame on the demuxer goroutine, so consuming there fires a session/load in the middle of another " +
			"load's replay — duplicating the session's whole history onto the broker")
	}
}

// TestEventFrameDoesNotConsumeTheCwdMissingRetry drives the actual demuxer path
// rather than the helper beneath it: an inbound gofer/event notification, the
// shape a re-init's own history replay arrives in, must not consume the arming.
// This is the exact sequence the defect fired on — the explicit re-init's replay
// frames landing while a retry from the earlier failed attach was still pending.
func TestEventFrameDoesNotConsumeTheCwdMissingRetry(t *testing.T) {
	const sid = "sess-1"
	r, rec := armedSession(t, sid)

	raw, err := json.Marshal(event.NewSessionCreated(sid))
	if err != nil {
		t.Fatalf("marshal session.created: %v", err)
	}
	r.handleNotification(daemon.Notification{Method: methodGoferEvent, Params: raw})

	if !rec.reload {
		t.Error("an inbound gofer/event frame consumed the cwd-missing retry — a re-init's replay would " +
			"trigger a second, blank-cwd session/load and replay the whole history again")
	}
}

// TestRegisterFreshSupersedesThePendingRetry pins the other half. A caller that
// calls RegisterFresh is declaring that IT is issuing this session's load
// (daemonbridge.Supervisor.Resume does exactly this, immediately before its
// own session/load), which supersedes the core's pending retry. Without the
// clear, the consumer's follow-up Subscribe after a successful re-init consumes
// the now-stale arming and loads the session a second time.
func TestRegisterFreshSupersedesThePendingRetry(t *testing.T) {
	const sid = "sess-1"
	r, rec := armedSession(t, sid)

	r.RegisterFresh(sid)

	if rec.reload {
		t.Error("RegisterFresh left the pending retry armed: the caller's own load has superseded it, so the " +
			"next Subscribe would load the session a second time and replay its history twice")
	}
	// And it did not otherwise disturb the entry it found.
	if got := r.session(sid); got != rec {
		t.Error("RegisterFresh replaced an existing session entry instead of finding it")
	}
}

// TestTakeReloadIsSingleShot pins that one arming answers ONE retry however many
// consumers reference the session: a TUI attach subscribes and then subscribes
// decisions, and two loads per attach would double the replay for the same
// reason the defect above did.
func TestTakeReloadIsSingleShot(t *testing.T) {
	const sid = "sess-1"
	r, rec := armedSession(t, sid)

	if !r.takeReload(rec) {
		t.Fatal("takeReload reported no armed retry; this test would prove nothing")
	}
	if r.takeReload(rec) {
		t.Error("takeReload reported the SAME arming twice — one failed load would issue two retries")
	}
	if rec.reload {
		t.Error("takeReload left the flag set")
	}
}
