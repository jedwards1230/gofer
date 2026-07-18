package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
	"github.com/jedwards1230/gofer/internal/worker"
)

// Timeouts bounding the router↔worker wire so one wedged worker never hangs a
// daemon request handler (or a spawn) indefinitely.
const (
	// workerSpawnTimeout bounds reading the worker's stdout handshake after
	// fork/exec — fork + Go runtime init + listener bind. Generous: a cold
	// machine under load can take a while, and a stuck spawn is torn down at
	// this bound rather than leaking a half-started worker.
	workerSpawnTimeout = 30 * time.Second
	// workerDialTimeout bounds the loopback WebSocket dial to a just-spawned
	// worker.
	workerDialTimeout = 10 * time.Second
	// wireCallTimeout bounds a control Call/Notify to a worker (set_model,
	// interrupt, kill, archive) so a wedged worker socket cannot block the
	// daemon handler that issued it.
	wireCallTimeout = 15 * time.Second
	// replyCallTimeout bounds the permission.reply Notify. [Supervisor.Reply]
	// takes no context (its interface signature has none), so it derives a
	// bounded one from context.Background rather than blocking forever on a
	// wedged worker socket.
	replyCallTimeout = 10 * time.Second
)

// Config configures a [Supervisor].
type Config struct {
	// Root is the shared session store root — passed to each worker via
	// --root, and read directly for the offline half of [Supervisor.List] and
	// for [Supervisor.History]. Empty resolves to gofer's own default (~/.gofer)
	// via [supervisor.ResolveRoot].
	Root string
	// Model is the default model new sessions resolve to when [supervisor.CreateOptions.Model]
	// is empty — passed to each worker via --model and onto the session/new
	// request.
	Model string
	// SelfExe is the path to the gofer executable to spawn as a worker. Empty
	// resolves via [os.Executable] at construction (unless NewWorkerCmd is set,
	// which bypasses selfExe entirely).
	SelfExe string
	// Logger receives the router's structured logs. Nil discards them.
	Logger *slog.Logger
	// NewWorkerCmd, when non-nil, replaces the default worker command builder
	// (exec.CommandContext(baseCtx, selfExe, "session-worker", "--root", …,
	// "--model", …)). It is the seam a test uses to spawn a faux-provider worker
	// process it can kill, in place of the real `gofer session-worker`. The
	// router owns the returned command's stdout pipe, Start, and Wait — the
	// builder only shapes the program, args, env, and dir.
	NewWorkerCmd func(ctx context.Context, model, cwd string) *exec.Cmd
}

// workerHandle is one live session's worker: its OS process, the client
// connection to it, and the reconstruction core rebuilding its event stream.
// A worker hosts exactly one session, so there is exactly one handle per session
// id and one reconstructed broker per handle — closing the handle's rec isolates
// that session's teardown from every sibling worker.
type workerHandle struct {
	id     string
	cmd    *exec.Cmd
	client *daemon.Client
	rec    *wirestream.Reconstructor

	// stopped marks an intentional teardown (Kill/Archive/Close) so the reaper
	// goroutine (see [Supervisor.reap]) does not treat the process exit as a
	// crash. Guarded by the owning [Supervisor]'s mu.
	stopped bool
}

// Supervisor runs each session in its own `gofer session-worker` child process
// and proxies the daemon's [daemon.Supervisor] calls to it over the public wire.
// See the package doc for the design and the Phase-1 cuts.
type Supervisor struct {
	root    string
	model   string
	selfExe string
	log     *slog.Logger

	// store enumerates on-disk sessions for List's offline half (store.List per
	// project slug — a directory read, never a cached journal). History uses a
	// throwaway store instead, for a fresh fold (see [Supervisor.History]).
	store *session.FileStore

	// baseCtx is the lifetime of every spawned worker: workers are launched with
	// exec.CommandContext(baseCtx, …), so cancelBase (called by Close) kills them
	// all. It is deliberately NOT a request context — a worker must outlive the
	// Create call that spawned it.
	baseCtx    context.Context
	cancelBase context.CancelFunc

	// newWorkerCmd is Config.NewWorkerCmd; nil uses buildDefaultWorkerCmd.
	newWorkerCmd func(ctx context.Context, model, cwd string) *exec.Cmd

	mu      sync.Mutex
	workers map[string]*workerHandle
	closed  bool

	// reapWG joins every per-worker reaper goroutine so Close returns leak-free.
	reapWG sync.WaitGroup
}

// gofer-native wire method literals the router calls on a worker. They mirror
// internal/daemon/handlers.go's own (unexported) constants — the same strings
// internal/daemonbridge hardcodes, since they ARE the daemon's public wire
// contract, not an internal detail.
const (
	methodGoferKill       = "gofer/kill"
	methodGoferArchive    = "gofer/archive"
	methodGoferSetModel   = "gofer/set_model"
	methodPermissionReply = "permission.reply"
)

// Supervisor satisfies the daemon's hosting interface — a signature drift fails
// the build here rather than at daemon.New.
var _ daemon.Supervisor = (*Supervisor)(nil)

// New builds a Supervisor. It resolves the store root and (unless a NewWorkerCmd
// seam is supplied) the gofer executable eagerly, so a bad root or an
// unresolvable executable fails at construction rather than on the first Create.
func New(cfg Config) (*Supervisor, error) {
	root, err := supervisor.ResolveRoot(cfg.Root)
	if err != nil {
		return nil, err
	}

	selfExe := cfg.SelfExe
	if selfExe == "" && cfg.NewWorkerCmd == nil {
		selfExe, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("router: resolve gofer executable: %w", err)
		}
	}

	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		return nil, fmt.Errorf("router: open session store: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	baseCtx, cancel := context.WithCancel(context.Background())
	return &Supervisor{
		root:         root,
		model:        cfg.Model,
		selfExe:      selfExe,
		log:          logger,
		store:        store,
		baseCtx:      baseCtx,
		cancelBase:   cancel,
		newWorkerCmd: cfg.NewWorkerCmd,
		workers:      make(map[string]*workerHandle),
	}, nil
}

// Create spawns a worker for a new session, dials it, creates the session on the
// worker over the wire, and registers the live handle. An empty prompt creates an
// idle session; a non-empty prompt is dispatched as the first turn (fire-and-forget,
// via the reconstruction core's Send). Only the returned [supervisor.SessionInfo.ID]
// is consumed by the daemon's session/new handler.
func (s *Supervisor) Create(ctx context.Context, prompt string, opts supervisor.CreateOptions) (supervisor.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return supervisor.SessionInfo{}, err
	}
	if s.isClosed() {
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: %w", ErrNotLive)
	}

	model := opts.Model
	if model == "" {
		model = s.model
	}

	cmd := s.buildWorkerCmd(s.baseCtx, model, opts.Cwd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: worker stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: start worker: %w", err)
	}
	// From here the process is live and MUST be reaped on any failure path.

	hsCtx, hsCancel := context.WithTimeout(ctx, workerSpawnTimeout)
	hs, err := readHandshake(hsCtx, stdout)
	hsCancel()
	if err != nil {
		killAndWait(cmd)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: read worker handshake: %w", err)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, workerDialTimeout)
	client, err := daemon.Dial(dialCtx, hs.Addr, "")
	dialCancel()
	if err != nil {
		killAndWait(cmd)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: dial worker %s: %w", hs.Addr, err)
	}
	rec := wirestream.New(client)

	// Bound the session/new Call like every other router→worker control RPC
	// (see methods.go): a worker that dialed but then wedged before answering
	// must not hang this Create — and with it the daemon handler for the whole
	// connection lifetime — which is the exact failure mode M6 isolation exists
	// to contain.
	newCtx, newCancel := context.WithTimeout(ctx, wireCallTimeout)
	raw, err := client.Call(newCtx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: opts.Cwd, Model: model})
	newCancel()
	if err != nil {
		_ = rec.Close()
		killAndWait(cmd)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: session/new on worker: %w", err)
	}
	var resp acp.NewSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		_ = rec.Close()
		killAndWait(cmd)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: decode %s response: %w", acp.MethodSessionNew, err)
	}
	uuid := resp.SessionID

	// Pre-register the fresh session as history-free BEFORE any SubscribeLive can
	// touch it: a first-reference SubscribeLive would otherwise trigger a
	// session/load history replay onto the live stream (see wirestream's
	// contract). RegisterFresh keeps the create/prompt path clean.
	rec.RegisterFresh(uuid)

	h := &workerHandle{id: uuid, cmd: cmd, client: client, rec: rec}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = rec.Close()
		killAndWait(cmd)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: %w", ErrNotLive)
	}
	s.workers[uuid] = h
	s.reapWG.Add(1)
	s.mu.Unlock()
	go s.reap(h)

	if prompt != "" {
		// Send ignores its ctx by design (wirestream contract); the turn outlives
		// this call and the worker drives it.
		_ = rec.Send(ctx, uuid, prompt)
	}

	s.log.Info("worker spawned", "session", uuid, "addr", hs.Addr, "pid", hs.PID)
	now := time.Now()
	status := supervisor.StatusNeedsInput
	if prompt != "" {
		status = supervisor.StatusWorking
	}
	return supervisor.SessionInfo{
		ID:      uuid,
		Model:   model,
		Cwd:     opts.Cwd,
		Status:  status,
		Created: now,
		Updated: now,
		Live:    true,
	}, nil
}

// buildWorkerCmd shapes the (unstarted) worker process command. The default
// execs `gofer session-worker --root <root> [--model <model>]` under baseCtx so
// the worker dies when the router closes; a test overrides it via
// Config.NewWorkerCmd.
func (s *Supervisor) buildWorkerCmd(ctx context.Context, model, cwd string) *exec.Cmd {
	if s.newWorkerCmd != nil {
		return s.newWorkerCmd(ctx, model, cwd)
	}
	args := []string{"session-worker", "--root", s.root}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, s.selfExe, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	// The worker writes ONLY its handshake line to stdout (which the router
	// captures via StdoutPipe); all logs go to stderr, forwarded to the router's
	// own stderr for this slice (per-worker log files are a Phase-2 concern).
	cmd.Stderr = os.Stderr
	return cmd
}

// readHandshake reads the worker's stdout until a line JSON-decodes into a
// non-empty [worker.Handshake] — the router's discovery step. It runs the
// blocking read on a goroutine and selects on ctx so a worker that never emits a
// handshake is bounded by the caller's timeout (the caller then kills the
// process, which closes stdout and unblocks the goroutine). On success it drains
// the rest of stdout to io.Discard for the worker's lifetime so the pipe never
// blocks the worker and cmd.Wait closes cleanly.
func readHandshake(ctx context.Context, stdout io.Reader) (worker.Handshake, error) {
	br := bufio.NewReader(stdout)
	type result struct {
		hs  worker.Handshake
		err error
	}
	// ch is buffered (cap 1) on purpose: on the ctx-timeout path below this
	// function returns while the reader goroutine is still blocked in
	// ReadBytes. Its caller (Create) then kills the worker on the handshake
	// error, which closes stdout and unblocks ReadBytes; the goroutine's final
	// send lands in the buffer with no reader and it exits — so it never leaks,
	// even though nothing reads ch after the timeout.
	ch := make(chan result, 1)
	go func() {
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				var hs worker.Handshake
				if uerr := json.Unmarshal(line, &hs); uerr == nil && hs.Addr != "" {
					ch <- result{hs: hs}
					return
				}
			}
			if err != nil {
				ch <- result{err: err}
				return
			}
		}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return worker.Handshake{}, r.err
		}
		// Honor os/exec's StdoutPipe contract: it is incorrect to call cmd.Wait
		// (which reap does) before all reads from the pipe complete. This drain
		// keeps reading until EOF (the worker's exit closes stdout), so Wait
		// never races an in-flight read on the pipe.
		go func() { _, _ = io.Copy(io.Discard, br) }()
		return r.hs, nil
	case <-ctx.Done():
		return worker.Handshake{}, ctx.Err()
	}
}

// reap waits for one worker's process to exit and reconciles the registry. An
// UNEXPECTED exit (stopped=false: a crash, OOM, or `kill -9`) drops the session's
// live handle and closes its reconstruction core — which terminates every
// attached subscriber's stream, so the daemon's in-flight prompt drain observes
// the close and returns a terminal error to the prompting peer (reconstructed as
// a fatal session.error client-side by wirestream). Every sibling worker is
// untouched: this handle's rec owns only this one session's broker. An INTENTIONAL
// exit (Kill/Archive/Close set stopped) is a quiet teardown.
func (s *Supervisor) reap(h *workerHandle) {
	defer s.reapWG.Done()
	waitErr := h.cmd.Wait()

	s.mu.Lock()
	intentional := h.stopped
	h.stopped = true
	if s.workers[h.id] == h {
		delete(s.workers, h.id)
	}
	s.mu.Unlock()

	// Close the reconstruction core (idempotent with wirestream's own
	// close-on-connection-drop): closes the client and every subscriber's stream.
	_ = h.rec.Close()

	if !intentional {
		s.log.Warn("worker exited unexpectedly; session now offline", "session", h.id, "err", waitErr)
	}
}

// Close terminates every worker, joins their reaper goroutines, and closes the
// enumeration store. Idempotent.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	handles := make([]*workerHandle, 0, len(s.workers))
	for _, h := range s.workers {
		h.stopped = true
		handles = append(handles, h)
	}
	s.workers = make(map[string]*workerHandle)
	s.mu.Unlock()

	// Cancel baseCtx first so exec.CommandContext SIGKILLs every worker, then
	// belt-and-suspenders each process directly (harmless once already dead).
	s.cancelBase()
	for _, h := range handles {
		_ = h.rec.Close()
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill()
		}
	}
	s.reapWG.Wait()
	return s.store.Close()
}

// get returns id's live handle without removing it.
func (s *Supervisor) get(id string) (*workerHandle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.workers[id]
	return h, ok
}

// take removes id from the registry and marks it intentionally stopped, so the
// reaper treats the subsequent process exit as a teardown rather than a crash.
func (s *Supervisor) take(id string) (*workerHandle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.workers[id]
	if !ok {
		return nil, false
	}
	h.stopped = true
	delete(s.workers, id)
	return h, true
}

// snapshotHandles copies the live handle set under the lock for lock-free
// iteration (roster aggregation).
func (s *Supervisor) snapshotHandles() []*workerHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*workerHandle, 0, len(s.workers))
	for _, h := range s.workers {
		out = append(out, h)
	}
	return out
}

// isClosed reports whether the supervisor has been closed.
func (s *Supervisor) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// killAndWait terminates a spawned worker whose registration failed before a
// reaper goroutine took ownership of its cmd.Wait, so the process is reaped
// exactly once (never double-Waited against a reaper).
func killAndWait(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}
