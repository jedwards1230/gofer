package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// testDaemon builds a real [daemon.Daemon] hosting a real [supervisor.Supervisor]
// (real journals, real event broker; newProvider stands in for a live model —
// no network) behind an in-process httptest server, returning a
// --daemon-flag-shaped address (bare host:port, no scheme — see
// [dialDaemon]/[wsURL]) a ps/kill/archive test points --daemon at.
func testDaemon(t *testing.T, token string, newProvider func() provider.Provider) string {
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
			opts.Provider = newProvider()
			return runner.New(ctx, opts)
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = newProvider()
			return runner.Resume(ctx, id, opts)
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	d := daemon.New(sup, daemon.Config{BearerToken: token, DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	// srv.URL is http://127.0.0.1:PORT; a --daemon flag wants a bare
	// host:port (matching daemon.Config.ListenAddr), which wsURL prefixes
	// with ws:// — strip the scheme rather than the http->ws swap
	// internal/daemon's own tests use, since here we're modeling what an
	// operator actually types.
	return strings.TrimPrefix(srv.URL, "http://")
}

func fauxProvider() provider.Provider { return faux.New(faux.Default()) }

// blockingProvider is a minimal hand-scripted [provider.Provider] whose first
// model call blocks until its ctx is cancelled — the seam
// TestArchiveRunningSessionTellsUserToKillFirst uses to deterministically put
// a session in the "running" state without any arbitrary sleep. It mirrors
// internal/daemon's own test seam of the same name; that one is unexported to
// its _test package, so this package needs its own copy.
type blockingProvider struct {
	started chan struct{}
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{})}
}

func (p *blockingProvider) Info() provider.ModelInfo { return provider.ModelInfo{ID: "block-test"} }

func (p *blockingProvider) Stream(ctx context.Context, _ provider.Request) (provider.StreamHandle, error) {
	return &blockingStream{p: p, ctx: ctx}, nil
}

type blockingStream struct {
	p   *blockingProvider
	ctx context.Context
	n   int
}

func (s *blockingStream) Next() (provider.StreamEvent, error) {
	s.n++
	if s.n == 1 {
		close(s.p.started)
		<-s.ctx.Done() // released only by session/cancel (Interrupt)
		return provider.StreamEvent{Type: provider.StreamTextDelta, Text: "hello"}, nil
	}
	return provider.StreamEvent{}, io.EOF
}

func (s *blockingStream) Close() error { return nil }

// sessionIDResult decodes the {"sessionId":"..."} shape shared by session/new
// and session/load responses.
type sessionIDResult struct {
	SessionID string `json:"sessionId"`
}

// newLiveSession creates one idle (needs-input) session directly through the
// running daemon at addr and returns its full id, so ps/kill/archive tests
// have something real to target without going through `gofer run`.
func newLiveSession(t *testing.T, ctx context.Context, addr, token string) string {
	t.Helper()
	c, err := daemon.Dial(ctx, addr, token)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	result, err := c.Call(ctx, "session/new", struct {
		Cwd string `json:"cwd"`
	}{t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var sess sessionIDResult
	if err := json.Unmarshal(result, &sess); err != nil {
		t.Fatalf("unmarshal session/new result: %v", err)
	}
	return sess.SessionID
}

// TestPSNoDaemon asserts `gofer ps` fails clearly (not a panic, not a hang)
// when no daemon is reachable, and that the failure names the daemon address
// the reader would use to fix it.
func TestPSNoDaemon(t *testing.T) {
	var out, errBuf bytes.Buffer
	got := run([]string{"ps", "--daemon", "127.0.0.1:1"}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(ps, no daemon) = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no gofer daemon running at 127.0.0.1:1") {
		t.Errorf("stderr = %q, want it to name the no-daemon condition and address", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "gofer daemon") {
		t.Errorf("stderr = %q, want a nudge toward `gofer daemon`", errBuf.String())
	}
}

// TestPSRostersLiveSession drives `gofer ps` end to end against a real daemon:
// the live roster shows the session it created, and --all still shows it
// (Live=true, since it was never archived).
func TestPSRostersLiveSession(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	ctx := context.Background()
	sid := newLiveSession(t, ctx, addr, "")

	var out, errBuf bytes.Buffer
	got := run([]string{"ps", "--daemon", addr}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run(ps) = %d, want 0\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(out.String(), shortID(sid)) {
		t.Errorf("ps output = %q, want it to contain the short id %q", out.String(), shortID(sid))
	}
	if !strings.Contains(out.String(), "needs-input") {
		t.Errorf("ps output = %q, want status needs-input", out.String())
	}

	var out2, errBuf2 bytes.Buffer
	got = run([]string{"ps", "--all", "--daemon", addr}, strings.NewReader(""), &out2, &errBuf2)
	if got != 0 {
		t.Fatalf("run(ps --all) = %d, want 0\nstderr: %s", got, errBuf2.String())
	}
	if !strings.Contains(out2.String(), "LIVE") || !strings.Contains(out2.String(), "true") {
		t.Errorf("ps --all output = %q, want a LIVE column showing true", out2.String())
	}
}

// TestKillDropsFromRosterKeepsJournal covers `gofer kill <shortid>`: the
// session drops off the live roster but a subsequent `gofer ps --all` still
// lists it (Live=false) — the journal is never deleted.
func TestKillDropsFromRosterKeepsJournal(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	ctx := context.Background()
	sid := newLiveSession(t, ctx, addr, "")

	var out, errBuf bytes.Buffer
	got := run([]string{"kill", shortID(sid), "--daemon", addr}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run(kill) = %d, want 0\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "killed") {
		t.Errorf("stderr = %q, want a kill confirmation", errBuf.String())
	}

	var psOut, psErr bytes.Buffer
	if got := run([]string{"ps", "--daemon", addr}, strings.NewReader(""), &psOut, &psErr); got != 0 {
		t.Fatalf("run(ps) after kill = %d\nstderr: %s", got, psErr.String())
	}
	if strings.Contains(psOut.String(), shortID(sid)) {
		t.Errorf("ps after kill = %q, want the killed session gone from the live roster", psOut.String())
	}

	var allOut, allErr bytes.Buffer
	if got := run([]string{"ps", "--all", "--daemon", addr}, strings.NewReader(""), &allOut, &allErr); got != 0 {
		t.Fatalf("run(ps --all) after kill = %d\nstderr: %s", got, allErr.String())
	}
	if !strings.Contains(allOut.String(), shortID(sid)) {
		t.Errorf("ps --all after kill = %q, want the killed session still listed (journal kept)", allOut.String())
	}
}

// TestKillUnknownID asserts an id with no matching live session fails clearly
// rather than silently succeeding.
func TestKillUnknownID(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)

	var out, errBuf bytes.Buffer
	got := run([]string{"kill", "deadbeef", "--daemon", addr}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(kill deadbeef) = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no live session matches") {
		t.Errorf("stderr = %q, want a clear no-match message", errBuf.String())
	}
}

// TestArchiveIdleSession covers `gofer archive <id>` on an idle session: it
// succeeds and the session drops from the live roster.
func TestArchiveIdleSession(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	ctx := context.Background()
	sid := newLiveSession(t, ctx, addr, "")

	var out, errBuf bytes.Buffer
	got := run([]string{"archive", shortID(sid), "--daemon", addr}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run(archive) = %d, want 0\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "archived") {
		t.Errorf("stderr = %q, want an archive confirmation", errBuf.String())
	}
}

// TestArchiveRunningSessionTellsUserToKillFirst covers the ErrRunning surface
// end to end through `gofer archive`: a session with a turn genuinely in
// flight (blockingProvider — no arbitrary sleep needed to win the race) is
// rejected, and the printed message tells the user to kill/interrupt it first
// rather than just repeating the daemon's raw error text.
func TestArchiveRunningSessionTellsUserToKillFirst(t *testing.T) {
	bp := newBlockingProvider()
	addr := testDaemon(t, "", func() provider.Provider { return bp })
	ctx := context.Background()

	c, err := daemon.Dial(ctx, addr, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	result, err := c.Call(ctx, "session/new", struct {
		Cwd string `json:"cwd"`
	}{t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var sess sessionIDResult
	if err := json.Unmarshal(result, &sess); err != nil {
		t.Fatalf("unmarshal session/new result: %v", err)
	}

	promptDone := make(chan struct{})
	go func() {
		_, _ = c.Call(ctx, "session/prompt", struct {
			SessionID string           `json:"sessionId"`
			Prompt    []map[string]any `json:"prompt"`
		}{sess.SessionID, []map[string]any{{"type": "text", "text": "hi"}}})
		close(promptDone)
	}()
	<-bp.started // the turn's first model call is genuinely blocked in flight

	var out, errBuf bytes.Buffer
	got := run([]string{"archive", shortID(sess.SessionID), "--daemon", addr}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(archive while running) = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "kill it") {
		t.Errorf("stderr = %q, want a kill-it-first hint", errBuf.String())
	}

	// Unblock the turn so the daemon-side goroutine settles cleanly before
	// the httptest server (and its supervisor) tear down.
	if err := c.Notify(ctx, "session/cancel", map[string]string{"sessionId": sess.SessionID}); err != nil {
		t.Fatalf("session/cancel: %v", err)
	}
	<-promptDone
}
