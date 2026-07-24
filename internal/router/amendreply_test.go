package router

// amendreply_test.go pins the permission.reply params [Supervisor.Reply]
// forwards to the owning worker. A router-backed session must amend
// identically to an in-process one, so an amended allow's replacement tool
// input has to cross the extra hop the router adds — and a plain allow must
// cross it as the exact bytes it did before amend existed, so a worker built
// before this change is unaffected.

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// startReplyCapturingWorker binds a worker socket + endpoint for id (like
// [startFakeWorker], so the router adopts it on construction) that answers
// gofer/hello and delivers the raw params of the first permission.reply
// NOTIFICATION it receives on the returned channel. Every other request gets
// the same prompt application error startFakeWorker sends, so an adopting
// router never blocks on a bounded Call.
func startReplyCapturingWorker(t *testing.T, id string) <-chan json.RawMessage {
	t.Helper()
	replies := make(chan json.RawMessage, 1)

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
				Params json.RawMessage `json:"params"`
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
			case env.Method == "permission.reply":
				select {
				case replies <- env.Params:
				default:
				}
			case len(env.ID) > 0:
				_ = wsjson.Write(r.Context(), c, jsonRPCError(env.ID, -32000, "fake worker: "+env.Method+" unimplemented"))
			}
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })

	writeEndpoint(t, id, os.Getpid(), daemon.WireVersion, "unix://"+sockPath)
	return replies
}

// TestReplyForwardsAmendedInputToWorker covers both wire shapes the worker
// hop must produce: a plain allow's params byte-for-byte unchanged (no
// "input" member at all — `omitempty` is what keeps an older worker
// unaffected), and an amended allow's params carrying the replacement input
// verbatim for the worker's own gate to substitute.
func TestReplyForwardsAmendedInputToWorker(t *testing.T) {
	tests := []struct {
		name       string
		op         event.PermissionReply
		wantParams string
	}{
		{
			name:       "plain allow omits input",
			op:         event.PermissionReply{ID: "call-1", Verdict: event.VerdictAllow},
			wantParams: `{"id":"call-1","verdict":"allow"}`,
		},
		{
			name: "amended allow carries the replacement input",
			op: event.PermissionReply{
				ID:       "call-1",
				Verdict:  event.VerdictAllow,
				Remember: true,
				Input:    json.RawMessage(`{"cmd":"rm -rf /tmp/x --dry-run","timeout":120}`),
			},
			wantParams: `{"id":"call-1","verdict":"allow","remember":true,"input":{"cmd":"rm -rf /tmp/x --dry-run","timeout":120}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortRuntimeDir(t)
			root := t.TempDir()
			id := uuid.Must(uuid.NewV7()).String()
			replies := startReplyCapturingWorker(t, id)

			sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
			if err != nil {
				t.Fatalf("router.New: %v", err)
			}
			t.Cleanup(func() {
				killWorkers(sup)
				_ = sup.Close()
			})
			if _, ok := sup.get(id); !ok {
				t.Fatalf("fake worker %s was not adopted", id)
			}

			if err := sup.Reply(id, tc.op); err != nil {
				t.Fatalf("Reply: %v", err)
			}

			select {
			case got := <-replies:
				if string(got) != tc.wantParams {
					t.Errorf("forwarded params = %s, want %s", got, tc.wantParams)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the forwarded permission.reply")
			}
		})
	}
}
