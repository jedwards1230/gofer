package daemon_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// syncBuffer is a concurrency-safe bytes.Buffer: slog's handler serializes
// its own Write calls internally, but this package's tests also read the
// buffer's contents (String) from the test goroutine while a request/
// notification handler goroutine may still be writing — a bare bytes.Buffer
// is not safe for that concurrent read/write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newLoggingTestDaemon is like newTestDaemon (see harness_test.go) but wires
// the daemon's logger to a buffer the test can inspect, at DEBUG level so
// every logged line — including notifications, which log at DEBUG — is
// captured for assertions.
func newLoggingTestDaemon(t *testing.T, sup *supervisor.Supervisor, token string) (string, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := daemon.New(sup, daemon.Config{BearerToken: token, DefaultModel: "faux", Logger: logger})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):], buf
}

// TestLogging_RequestOutcomeAndSessionLifecycle covers handleFrame's
// per-request logging (method/id/outcome) and handleSessionNew's lifecycle
// log, both at INFO.
func TestLogging_RequestOutcomeAndSessionLifecycle(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url, buf := newLoggingTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	logs := buf.String()
	if !strings.Contains(logs, `msg="request handled"`) {
		t.Errorf("logs missing request-handled line:\n%s", logs)
	}
	if !strings.Contains(logs, "method="+acp.MethodSessionNew) {
		t.Errorf("logs missing method=%s:\n%s", acp.MethodSessionNew, logs)
	}
	if !strings.Contains(logs, "outcome=ok") {
		t.Errorf("logs missing outcome=ok:\n%s", logs)
	}
	if !strings.Contains(logs, "dur_ms=") {
		t.Errorf("logs missing dur_ms:\n%s", logs)
	}
	if !strings.Contains(logs, `msg="session created"`) || !strings.Contains(logs, "session="+sid) {
		t.Errorf("logs missing session-created line for session=%s:\n%s", sid, logs)
	}
}

// TestLogging_UnknownMethodLogsAtWarn covers the methodNotFound branch:
// logged at WARN with the offending method name, per the redaction rule's
// client-compat-debugging carve-out.
func TestLogging_UnknownMethodLogsAtWarn(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url, buf := newLoggingTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request("bogus/method", nil)
	if resp.Error == nil {
		t.Fatal("bogus/method: got nil error, want methodNotFound")
	}

	logs := buf.String()
	if !strings.Contains(logs, "level=WARN") {
		t.Errorf("logs missing a WARN line:\n%s", logs)
	}
	if !strings.Contains(logs, `msg="unknown method"`) {
		t.Errorf("logs missing \"unknown method\":\n%s", logs)
	}
	if !strings.Contains(logs, "method=bogus/method") {
		t.Errorf("logs missing method=bogus/method:\n%s", logs)
	}
}

// TestLogging_NoParamsOrPromptContentLeak is the redaction test: a
// session/prompt carrying a sentinel string in its prompt text must never
// have that sentinel appear anywhere in the logger's output — handleFrame
// logs method/id/outcome/duration only, never env.Params or a handler's
// result.
func TestLogging_NoParamsOrPromptContentLeak(t *testing.T) {
	const sentinel = "sentinel-prompt-text-must-not-be-logged-8f3a1c"

	sup := newTestSupervisor(t, fauxProvider)
	url, buf := newLoggingTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp := c.request(acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock(sentinel)},
	})
	if resp.Error != nil {
		t.Fatalf("session/prompt error: %+v", resp.Error)
	}

	if strings.Contains(buf.String(), sentinel) {
		t.Fatalf("logs leaked prompt content:\n%s", buf.String())
	}
}

// TestLogging_NotificationLogsAtDebug covers session/cancel (sent as a
// notification, no id) logging at DEBUG rather than INFO.
func TestLogging_NotificationLogsAtDebug(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url, buf := newLoggingTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())
	c.notify(acp.MethodSessionCancel, acp.CancelNotification{SessionID: sid})

	// notify is fire-and-forget (no response to synchronize on), and its
	// handler runs on its own dispatch goroutine — poll rather than assume
	// ordering against anything else on the connection.
	logs := waitForLog(t, buf, `msg="notification handled"`)
	if !strings.Contains(logs, "method="+acp.MethodSessionCancel) {
		t.Errorf("logs missing method=%s:\n%s", acp.MethodSessionCancel, logs)
	}
	if !strings.Contains(logs, "level=DEBUG") {
		t.Errorf("logs missing a DEBUG line:\n%s", logs)
	}
}

// waitForLog polls buf until its contents contain substr or defaultWait
// elapses, returning the final contents either way (so a timeout's failure
// message still shows whatever was captured).
func waitForLog(t *testing.T, buf *syncBuffer, substr string) string {
	t.Helper()
	deadline := time.Now().Add(defaultWait)
	for {
		logs := buf.String()
		if strings.Contains(logs, substr) {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log line containing %q; got:\n%s", substr, logs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
