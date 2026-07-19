package router

// killreply_test.go pins the gofer/kill REPLY contract: a worker that fails to
// end the session cleanly is reported to the caller, and the teardown happens
// anyway. The in-process daemon surfaced that failure through Kill's return; in
// worker mode the reply is the only channel left for it, so discarding it would
// silently lose the signal.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// killReplyMode selects how the fake worker in [startKillReplyWorker] answers
// gofer/kill — the two ways a reply can fail to arrive intact.
type killReplyMode int

const (
	// killReplyErrors answers gofer/kill with a JSON-RPC application error, as a
	// worker does when it cannot finish the session cleanly.
	killReplyErrors killReplyMode = iota
	// killReplyDropped closes the connection with the request in flight, the
	// transport half of the same failure. It stands in for the [wireCallCtx]
	// deadline row too: a bound that expires and a socket that dies both surface
	// as a non-nil Call error into the exact same branch, and this one is
	// immediate rather than a 15s wall-clock wait.
	killReplyDropped
)

// killReplyErrMsg is the worker-side failure text a failing reply carries, so
// the assertion can prove the REPLY's error (not a generic one) reached the
// caller.
const killReplyErrMsg = "worker: could not finish session"

// startKillReplyWorker binds a worker socket + endpoint for id (like
// [startFakeWorker], so the router adopts it on construction) whose gofer/kill
// reply fails per mode. pid is the endpoint-advertised pid the adopted kill path
// SIGKILLs — pass a [parkedPID] so the test does not signal itself.
func startKillReplyWorker(t *testing.T, id string, pid int, mode killReplyMode) {
	t.Helper()
	sockPath, err := daemon.WorkerSocketPath(id)
	if err != nil {
		t.Fatalf("WorkerSocketPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir workers dir: %v", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		for {
			var env struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if err := wsjson.Read(r.Context(), c, &env); err != nil {
				return
			}
			switch {
			case env.Method == "gofer/hello":
				_ = wsjson.Write(r.Context(), c, jsonRPCResult(env.ID, daemon.HelloResult{
					BinaryVersion:      "test",
					WireVersion:        daemon.WireVersion,
					ACPProtocolVersion: 1,
				}))
			case env.Method == methodGoferKill:
				if mode == killReplyDropped {
					return // hang up mid-request; the pending Call fails
				}
				_ = wsjson.Write(r.Context(), c, jsonRPCError(env.ID, -32000, killReplyErrMsg))
			case len(env.ID) > 0:
				// Same as startFakeWorker: error every other request so an
				// adopting router never blocks on a bounded Call.
				_ = wsjson.Write(r.Context(), c, jsonRPCError(env.ID, -32000, "fake worker: "+env.Method+" unimplemented"))
			}
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })

	writeEndpoint(t, id, pid, daemon.WireVersion, "unix://"+sockPath)
}

// TestKillSurfacesReplyErrorAndStillKills is the regression test for the worker
// mode kill-reply drop: [Supervisor.Kill] discarded the gofer/kill reply
// (`_, _ = h.client.Call(...)`), so a worker reporting that it could not finish
// the session cleanly was silently ignored.
//
// Both rows assert the pair of properties that make the fix correct — the error
// is SURFACED, and the teardown still COMPLETED anyway (process SIGKILLed,
// handle gone from the live set). Copying Archive's return-before-teardown shape
// would satisfy the first and break the second, stranding a live worker behind a
// caller told the session is dead.
func TestKillSurfacesReplyErrorAndStillKills(t *testing.T) {
	tests := []struct {
		name    string
		mode    killReplyMode
		wantMsg string
	}{
		{
			name:    "worker answers gofer/kill with an application error",
			mode:    killReplyErrors,
			wantMsg: killReplyErrMsg,
		},
		{
			name: "worker hangs up with gofer/kill in flight",
			mode: killReplyDropped,
			// Transport failures carry no worker text; the wrapper prefix is the
			// only stable part, and it is what proves the error was not dropped.
			wantMsg: "router: kill",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortRuntimeDir(t)
			root := t.TempDir()
			id := uuid.Must(uuid.NewV7()).String()
			pid, waitKilled := parkedPID(t)
			startKillReplyWorker(t, id, pid, tc.mode)

			sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
			if err != nil {
				t.Fatalf("router.New: %v", err)
			}
			t.Cleanup(func() {
				killWorkers(sup)
				_ = sup.Close()
			})

			h, ok := sup.get(id)
			if !ok {
				t.Fatalf("fake worker %s was not adopted", id)
			}
			if h.pid != pid {
				t.Fatalf("adopted handle pid = %d, want the endpoint-advertised %d", h.pid, pid)
			}

			killErr := sup.Kill(context.Background(), id)

			// 1. The reply's error reached the caller instead of being dropped.
			if killErr == nil {
				t.Fatalf("Kill returned nil for a failing gofer/kill reply; the error was swallowed")
			}
			if !strings.Contains(killErr.Error(), tc.wantMsg) {
				t.Errorf("Kill error = %q, want it to carry %q", killErr, tc.wantMsg)
			}

			// 2. The kill still completed: the handle is out of the live set and
			// the worker process was signalled. A failing reply must never abort
			// the teardown.
			if _, live := sup.get(id); live {
				t.Errorf("session %s still live after Kill; the failing reply aborted the teardown", id)
			}
			waitKilled()
		})
	}
}
