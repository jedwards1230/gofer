package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
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
	// (exec.Command(selfExe, "session-worker", "--session", …, "--root", …,
	// "--model", …)). It is the seam a test uses to spawn a faux-provider worker
	// process it can kill, in place of the real `gofer session-worker`. The
	// builder only shapes the program, args, env, and dir; the router detaches
	// and starts it via [daemon.SpawnDetached] (Setsid + per-worker log) and
	// owns its lifecycle via [daemon.Reap]. It receives the router-pinned session
	// uuid so the faux worker can key its socket/endpoint/lock and pin its
	// session id to the same value the router expects (design Option A).
	NewWorkerCmd func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd
}

// workerHandle is one live session's worker: its OS process, the client
// connection to it, and the reconstruction core rebuilding its event stream.
// A worker hosts exactly one session, so there is exactly one handle per session
// id and one reconstructed broker per handle — closing the handle's rec isolates
// that session's teardown from every sibling worker.
//
// A handle is either SPAWNED (this router forked/execed the worker: cmd is set,
// and wait is its [daemon.Reap] channel) or ADOPTED (this router discovered an
// already-running detached worker on startup: cmd is NIL — there is no
// *exec.Cmd to Wait on for a process we did not start — and wait instead fires
// when the client connection closes, the only crash signal a router has for a
// worker it holds by socket alone). Every field access that differs between the
// two is guarded on cmd (kill signals the pid for an adopted worker) — see
// killHandleProcess.
type workerHandle struct {
	id     string
	cmd    *exec.Cmd
	client *daemon.Client
	rec    *wirestream.Reconstructor

	// pid is the worker process id: cmd.Process.Pid for a spawned worker, or the
	// endpoint file's advertised PID for an adopted one. It is the only handle a
	// router has on an ADOPTED worker's process (no *exec.Cmd), used for a
	// best-effort SIGKILL on Kill/Archive (see killHandleProcess).
	pid int

	// wait delivers the worker's exit: for a SPAWNED worker, [daemon.Reap]'s
	// cmd.Wait result (the sole owner of cmd.Wait — the reaper must never call
	// cmd.Wait directly); for an ADOPTED worker, a nil sent when the client
	// connection closes (its process died or we closed it). Buffered cap 1 in
	// both cases, so it delivers even after the router stops caring at shutdown.
	wait <-chan error

	// stopped marks an intentional teardown (Kill/Archive) so the reaper
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

	// newWorkerCmd is Config.NewWorkerCmd; nil uses buildWorkerCmd's default.
	newWorkerCmd func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd

	mu      sync.Mutex
	workers map[string]*workerHandle
	closed  bool

	// permRelay bridges an adopted session's reconstructed permission stream back
	// into the OUTER daemon's fan-out (route + broadcast), set once via
	// [Supervisor.SetPermissionRelay] after the daemon is constructed (design §7).
	// Nil until set — a router used without a daemon (some tests) simply runs no
	// permission watchers. Guarded by mu.
	permRelay daemon.PermissionRelay
	// watcherWG joins every adopted-session permission watcher goroutine so Close
	// returns leak-free (F4). Watchers exit when their broker closes (reap /
	// Close) or reaperStop fires.
	watcherWG sync.WaitGroup

	// reaperStop is closed once by Close to wake every per-worker reaper WITHOUT
	// killing its (detached) worker: a router shutdown deliberately abandons its
	// workers, which reparent to pid 1 and keep running (design §3). A reaper
	// selects on it so Close never blocks on a live worker's cmd.Wait.
	reaperStop chan struct{}
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

	s := &Supervisor{
		root:         root,
		model:        cfg.Model,
		selfExe:      selfExe,
		log:          logger,
		store:        store,
		newWorkerCmd: cfg.NewWorkerCmd,
		workers:      make(map[string]*workerHandle),
		reaperStop:   make(chan struct{}),
	}

	// Adopt any still-alive detached workers left by a PRIOR router (design §4):
	// a router restart re-attaches to in-flight sessions rather than orphaning
	// them, which is the whole point of M6 process isolation. Stale leftovers
	// from crashed workers are garbage-collected in the same scan. Best-effort:
	// a scan failure never fails construction — a router with an empty roster is
	// still a usable router (every Create spawns fresh).
	s.adoptExistingWorkers()

	return s, nil
}

// Create spawns a DETACHED worker for a new session, dials it, creates the
// session on the worker over the wire, and registers the live handle. An empty
// prompt creates an idle session; a non-empty prompt is dispatched as the first
// turn (fire-and-forget, via the reconstruction core's Send). Only the returned
// [supervisor.SessionInfo.ID] is consumed by the daemon's session/new handler.
//
// The session uuid is pre-generated here (design Option A) so the worker keys
// its socket/endpoint/lock by it and pins it as its session id; session/new on
// the worker must echo that exact id back. Because every Create draws a FRESH
// uuid, it can never collide with an orphaned prior worker's <uuid>.lock — the
// lock only blocks a duplicate worker for the SAME session id.
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

	// Pre-generate the session uuid the worker keys its files by and pins as its
	// session id (design Option A).
	sessionID := uuid.Must(uuid.NewV7()).String()

	// The worker writes its per-session runtime files (socket/endpoint/lock)
	// under the workers dir; its stdout+stderr go to a per-worker log file
	// there. Create the dir so SpawnDetached can open the log.
	workersDir, err := daemon.WorkersDir()
	if err != nil {
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: %w", err)
	}
	if err := os.MkdirAll(workersDir, 0o700); err != nil {
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: create workers dir: %w", err)
	}
	logPath := filepath.Join(workersDir, sessionID+".log")

	// Spawn detached (Setsid): the worker outlives a router restart (design §3).
	// context.Background, not a Close-cancelled context — a detached worker must
	// NOT be torn down when the router shuts down.
	cmd := s.buildWorkerCmd(context.Background(), sessionID, model, opts.Cwd)
	pid, err := daemon.SpawnDetached(cmd, logPath)
	if err != nil {
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: spawn worker: %w", err)
	}
	// From here the process is live and MUST be reaped on any failure path.
	// daemon.Reap is the SOLE owner of cmd.Wait; terminate drains it.
	waitCh := daemon.Reap(cmd)

	// Discover the worker by POLLING its endpoint file (the uuid we generated
	// keys it), which the worker writes atomically just before it serves — the
	// same mechanism the adoption slice will use, so both discovery paths
	// converge on one structured artifact. The worker's stdout handshake line is
	// kept as an informational log artifact, not parsed here.
	readyCtx, readyCancel := context.WithTimeout(ctx, workerSpawnTimeout)
	ep, exited, err := waitForWorkerEndpoint(readyCtx, sessionID, waitCh)
	readyCancel()
	if err != nil {
		// If the worker already exited, waitForWorkerEndpoint consumed its wait
		// result — do NOT terminate (a second drain would block forever) — but
		// still sweep any endpoint/socket a wedged-then-dead worker left behind.
		if exited {
			removeWorkerArtifacts(sessionID)
		} else {
			cleanupSpawnedWorker(sessionID, cmd, waitCh)
		}
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: await worker endpoint: %w", err)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, workerDialTimeout)
	client, err := daemon.Dial(dialCtx, ep.Addr, "")
	dialCancel()
	if err != nil {
		cleanupSpawnedWorker(sessionID, cmd, waitCh)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: dial worker %s: %w", ep.Addr, err)
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
		cleanupSpawnedWorker(sessionID, cmd, waitCh)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: session/new on worker: %w", err)
	}
	var resp acp.NewSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		_ = rec.Close()
		cleanupSpawnedWorker(sessionID, cmd, waitCh)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: decode %s response: %w", acp.MethodSessionNew, err)
	}
	// The worker MUST echo the pinned uuid as its session id (design Option A).
	// A mismatch means the pinning bridge is broken (an unpinned worker binary),
	// which would register a handle keyed by an id the runtime files are not
	// keyed by — fail loudly rather than silently desync.
	if resp.SessionID != sessionID {
		_ = rec.Close()
		cleanupSpawnedWorker(sessionID, cmd, waitCh)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: worker session id %q != pinned %q", resp.SessionID, sessionID)
	}

	// Pre-register the fresh session as history-free BEFORE any SubscribeLive can
	// touch it: a first-reference SubscribeLive would otherwise trigger a
	// session/load history replay onto the live stream (see wirestream's
	// contract). RegisterFresh keeps the create/prompt path clean.
	rec.RegisterFresh(sessionID)

	h := &workerHandle{id: sessionID, cmd: cmd, client: client, rec: rec, pid: pid, wait: waitCh}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = rec.Close()
		cleanupSpawnedWorker(sessionID, cmd, waitCh)
		return supervisor.SessionInfo{}, fmt.Errorf("router: create: %w", ErrNotLive)
	}
	s.workers[sessionID] = h
	s.reapWG.Add(1)
	s.mu.Unlock()
	go s.reap(h)

	if prompt != "" {
		// Send ignores its ctx by design (wirestream contract); the turn outlives
		// this call and the worker drives it.
		_ = rec.Send(ctx, sessionID, prompt)
	}

	s.log.Info("worker spawned", "session", sessionID, "addr", ep.Addr, "pid", pid)
	now := time.Now()
	status := supervisor.StatusNeedsInput
	if prompt != "" {
		status = supervisor.StatusWorking
	}
	return supervisor.SessionInfo{
		ID:      sessionID,
		Model:   model,
		Cwd:     opts.Cwd,
		Status:  status,
		Created: now,
		Updated: now,
		Live:    true,
	}, nil
}

// buildWorkerCmd shapes the (unstarted) worker process command. The default
// execs `gofer session-worker --session <uuid> --root <root> [--model <model>]`;
// a test overrides it via Config.NewWorkerCmd. Stdio is deliberately left unset
// here — [daemon.SpawnDetached] redirects stdout+stderr to the per-worker log
// file at Start.
func (s *Supervisor) buildWorkerCmd(ctx context.Context, sessionID, model, cwd string) *exec.Cmd {
	if s.newWorkerCmd != nil {
		return s.newWorkerCmd(ctx, sessionID, model, cwd)
	}
	args := []string{"session-worker", "--session", sessionID, "--root", s.root}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, s.selfExe, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	return cmd
}

// endpointPollInterval bounds how often waitForWorkerEndpoint re-reads the
// worker's endpoint file while the worker is starting.
const endpointPollInterval = 15 * time.Millisecond

// waitForWorkerEndpoint polls for sessionID's endpoint file — written
// atomically by the worker just before it serves ([daemon.WriteWorkerEndpoint])
// — and returns it once it appears. This is the router's fresh-spawn discovery
// step, the same structured mechanism the adoption slice reuses. Readiness is
// gated on exactly three outcomes: the endpoint appears (success), the worker
// process exits first (fast-fail), or ctx expires (timeout) — never on a
// signal-0 pid probe, whose pid could be reused.
//
// The returned bool reports whether the worker already exited: in that case its
// wait result was CONSUMED here, so the caller must not drain waitCh again.
func waitForWorkerEndpoint(ctx context.Context, sessionID string, wait <-chan error) (daemon.WorkerEndpoint, bool, error) {
	ticker := time.NewTicker(endpointPollInterval)
	defer ticker.Stop()
	for {
		ep, err := daemon.ReadWorkerEndpoint(sessionID)
		if err == nil {
			return ep, false, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			// A genuine read/parse error, not just "not written yet".
			return daemon.WorkerEndpoint{}, false, fmt.Errorf("read worker endpoint: %w", err)
		}
		select {
		case werr := <-wait:
			if werr != nil {
				return daemon.WorkerEndpoint{}, true, fmt.Errorf("worker exited before advertising its endpoint: %w", werr)
			}
			return daemon.WorkerEndpoint{}, true, errors.New("worker exited before advertising its endpoint")
		case <-ctx.Done():
			return daemon.WorkerEndpoint{}, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

// reap waits for one worker's detached process to exit (via its [daemon.Reap]
// channel) and reconciles the registry — OR returns early when Close signals a
// router shutdown, deliberately abandoning the still-alive detached worker.
//
// An UNEXPECTED exit (stopped=false: a crash, OOM, or `kill -9`) drops the
// session's live handle and closes its reconstruction core — which terminates
// every attached subscriber's stream, so the daemon's in-flight prompt drain
// observes the close and returns a terminal error to the prompting peer
// (reconstructed as a fatal session.error client-side by wirestream). Every
// sibling worker is untouched: this handle's rec owns only this one session's
// broker. An INTENTIONAL exit (Kill/Archive set stopped) is a quiet teardown.
func (s *Supervisor) reap(h *workerHandle) {
	defer s.reapWG.Done()

	var waitErr error
	select {
	case waitErr = <-h.wait:
		// The worker process exited.
	case <-s.reaperStop:
		// Router shutdown: abandon this DETACHED worker (design §3). It keeps
		// running, reparents to pid 1, and is reaped there. The NEXT router start
		// re-adopts it by socket scan (see [Supervisor.adoptExistingWorkers]).
		return
	}

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

// Close stops the router's supervision WITHOUT killing its workers, joins the
// reaper goroutines, and closes the enumeration store. Idempotent.
//
// Detached workers deliberately survive a router shutdown (design §3): they
// reparent to pid 1 and keep pumping their in-flight turns. This is the whole
// point of M6 process isolation. The NEXT router start re-adopts them by socket
// scan (see [Supervisor.adoptExistingWorkers]) — each is still holding its unix
// socket, <uuid>.lock, and <uuid>.json endpoint, which the scan reads to dial and
// re-attach. Between shutdown and that next start the workers are simply
// unattached-but-alive.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	handles := make([]*workerHandle, 0, len(s.workers))
	for _, h := range s.workers {
		handles = append(handles, h)
	}
	s.workers = make(map[string]*workerHandle)
	s.mu.Unlock()

	// Wake every reaper and permission watcher without killing any worker; a
	// reaper blocked on a live worker's exit takes the reaperStop branch and
	// returns, so reapWG.Wait never hangs on a detached worker that outlives us.
	close(s.reaperStop)
	s.reapWG.Wait()

	// Release the router's own client connections (does NOT stop the workers).
	// Closing each rec closes its broker, which closes every permission watcher's
	// subscription so watcherWG.Wait below cannot hang.
	for _, h := range handles {
		_ = h.rec.Close()
	}
	s.watcherWG.Wait()
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

// terminate kills a spawned worker whose registration failed before a reaper
// goroutine took ownership, and drains its single [daemon.Reap] result so the
// process is reaped exactly once (Reap is the sole owner of cmd.Wait — a direct
// cmd.Wait here would be a forbidden second Wait).
func terminate(cmd *exec.Cmd, wait <-chan error) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-wait
}
