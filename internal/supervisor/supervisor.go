package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/runner"
)

// Config configures a [Supervisor].
type Config struct {
	// Root is the shared session store's root directory. Empty uses the SDK
	// default (~/.gofer).
	Root string
	// Clock overrides the wall clock used to timestamp roster entries.
	// Defaults to time.Now. Test seam.
	Clock func() time.Time

	// Store, when set, is used instead of building a store from Root, and is
	// NOT closed by [Supervisor.Close] — the caller owns its lifecycle. Test
	// seam.
	Store *session.FileStore
	// NewSession, when set, replaces the default construction of a fresh
	// session (which calls [runner.NewSession] with the shared store
	// injected). Test seam.
	NewSession func(ctx context.Context, opts runner.Options) (Session, error)
	// ResumeSession, when set, replaces the default reopening of an existing
	// session (which calls [runner.Resume] with the shared store injected).
	// Test seam.
	ResumeSession func(ctx context.Context, id string, opts runner.Options) (Session, error)
}

// Supervisor is a concurrency-safe registry of live sessions over one shared
// session store. See the package doc for the full contract.
type Supervisor struct {
	root      string
	store     *session.FileStore
	ownsStore bool
	clock     func() time.Time

	newSession    func(ctx context.Context, opts runner.Options) (Session, error)
	resumeSession func(ctx context.Context, id string, opts runner.Options) (Session, error)

	// resumeMu serializes Resume end-to-end (roster check through
	// registration) so two concurrent Resumes of the same id can never both
	// observe "not live" and both build a second runner over the same
	// on-disk journal — the SDK's store caches one live journal per id, and
	// two runners driving it would race on appends.
	resumeMu sync.Mutex

	mu     sync.Mutex
	roster map[string]*managed
	closed bool

	// watchMu guards the WatchRoster subscriber registry and its shutdown
	// flag, independent of mu so notify's fan-out never contends with roster
	// bookkeeping. watchDone is closed once by Close to wake every watcher;
	// watchWG joins their goroutines so Close returns leak-free.
	watchMu     sync.Mutex
	watchers    map[*watcher]struct{}
	watchClosed bool
	watchDone   chan struct{}
	watchWG     sync.WaitGroup
}

// New builds a Supervisor. It opens (or accepts, via [Config.Store]) the
// shared session store eagerly, so a bad root fails at construction rather
// than on the first Create.
func New(cfg Config) (*Supervisor, error) {
	root, err := resolveRoot(cfg.Root)
	if err != nil {
		return nil, err
	}

	store := cfg.Store
	ownsStore := false
	if store == nil {
		var storeOpts []session.StoreOption
		if cfg.Root != "" {
			storeOpts = append(storeOpts, session.WithRoot(cfg.Root))
		}
		store, err = session.NewFileStore(storeOpts...)
		if err != nil {
			return nil, fmt.Errorf("supervisor: open session store: %w", err)
		}
		ownsStore = true
	}

	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	newSession := cfg.NewSession
	if newSession == nil {
		newSession = func(ctx context.Context, opts runner.Options) (Session, error) {
			opts.Store = store
			return runner.NewSession(ctx, opts)
		}
	}
	resumeSession := cfg.ResumeSession
	if resumeSession == nil {
		resumeSession = func(ctx context.Context, id string, opts runner.Options) (Session, error) {
			opts.Store = store
			return runner.Resume(ctx, id, opts)
		}
	}

	return &Supervisor{
		root:          root,
		store:         store,
		ownsStore:     ownsStore,
		clock:         clock,
		newSession:    newSession,
		resumeSession: resumeSession,
		roster:        make(map[string]*managed),
		watchers:      make(map[*watcher]struct{}),
		watchDone:     make(chan struct{}),
	}, nil
}

// resolveRoot mirrors the SDK FileStore's default-root resolution (~/.gofer)
// so the supervisor can enumerate <root>/sessions itself in List — the SDK
// exposes no store-wide "list every session" call.
func resolveRoot(root string) (string, error) {
	if root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("supervisor: resolve default store root: %w", err)
	}
	return filepath.Join(home, ".gofer"), nil
}

// CreateOptions configures [Supervisor.Create]. The zero value is valid: an
// empty Model resolves to the credential-driven default, an empty Cwd to the
// daemon's working directory (the caller's responsibility upstream), and a
// zero MaxIters to the loop default.
type CreateOptions struct {
	Model    string
	Cwd      string
	System   string
	Params   provider.Params
	MaxIters int
}

// Create starts a fresh session and registers it live. An empty prompt
// creates an idle session with no first turn (the ACP session/new path); a
// non-empty prompt is enqueued as the session's first turn.
func (s *Supervisor) Create(ctx context.Context, prompt string, opts CreateOptions) (SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, err
	}
	if s.isClosed() {
		return SessionInfo{}, ErrClosed
	}
	sess, err := s.newSession(ctx, runner.Options{
		Root: s.root, Cwd: opts.Cwd, Model: opts.Model, System: opts.System,
		Params: opts.Params, MaxIters: opts.MaxIters,
	})
	if err != nil {
		return SessionInfo{}, fmt.Errorf("supervisor: create session: %w", err)
	}

	m, err := s.register(sess, opts.Model)
	if err != nil {
		// Lost a race with Close between the isClosed check above and here:
		// tear down the just-built session so it does not leak. Its store is
		// the shared one and stays open; only its broker and journal close.
		_ = sess.Close()
		return SessionInfo{}, fmt.Errorf("supervisor: create session: %w", err)
	}
	if prompt != "" {
		if err := m.enqueue(prompt); err != nil {
			return SessionInfo{}, fmt.Errorf("supervisor: create session: enqueue first prompt: %w", err)
		}
	} else {
		// enqueue announces the session on the prompt path; announce the new
		// idle session here on the no-prompt (ACP session/new) path.
		s.notify()
	}
	return m.info(), nil
}

// ResumeOptions configures [Supervisor.Resume]. Model and Cwd are required —
// the journal itself does not persist them.
type ResumeOptions struct {
	Cwd, Model, System string
	Params             provider.Params
	MaxIters           int
}

// Resume reopens an on-disk session and registers it live. If id is already
// live, Resume is a no-op that returns the existing snapshot — it never
// builds a second runner over the same journal.
func (s *Supervisor) Resume(ctx context.Context, id string, opts ResumeOptions) (SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, err
	}

	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()

	if m, ok := s.get(id); ok {
		return m.info(), nil
	}
	if s.isClosed() {
		return SessionInfo{}, ErrClosed
	}

	sess, err := s.resumeSession(ctx, id, runner.Options{
		Root: s.root, Cwd: opts.Cwd, Model: opts.Model, System: opts.System,
		Params: opts.Params, MaxIters: opts.MaxIters,
	})
	if err != nil {
		return SessionInfo{}, fmt.Errorf("supervisor: resume %s: %w", id, err)
	}

	m, err := s.register(sess, opts.Model)
	if err != nil {
		_ = sess.Close()
		return SessionInfo{}, fmt.Errorf("supervisor: resume %s: %w", id, err)
	}
	info := m.info()
	s.notify()
	return info, nil
}

// register adds sess to the roster as a live, idle session and starts its
// pump goroutine. It returns ErrClosed — checked under s.mu, atomically with
// the roster insert — if the supervisor has been closed, so a Create/Resume
// racing Close can never insert a session (and leak its pump) into a roster
// Close has already drained. The managed value (and its context) is built
// only once the insert is committed, so a rejected registration leaks nothing.
func (s *Supervisor) register(sess Session, model string) (*managed, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	m := newManaged(sess, model, s.clock(), s.clock, s.notify)
	s.roster[m.id] = m
	s.mu.Unlock()

	go m.pump()
	return m, nil
}

// Send enqueues a prompt for id. It dispatches immediately when the session
// is idle, else queues FIFO — real steering, never reject-if-busy.
func (s *Supervisor) Send(ctx context.Context, sessionID, prompt string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, err := s.lookup(sessionID)
	if err != nil {
		return fmt.Errorf("supervisor: send %s: %w", sessionID, err)
	}
	if err := m.enqueue(prompt); err != nil {
		return fmt.Errorf("supervisor: send %s: %w", sessionID, err)
	}
	return nil
}

// QueueList returns the pending (not yet dispatched) prompt texts for id, in
// FIFO order.
func (s *Supervisor) QueueList(ctx context.Context, id string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := s.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("supervisor: queue list %s: %w", id, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.queue))
	copy(out, m.queue)
	return out, nil
}

// QueueClear drops every pending prompt for id (it does not interrupt a
// turn already in flight) and returns how many were cleared.
func (s *Supervisor) QueueClear(ctx context.Context, id string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m, err := s.lookup(id)
	if err != nil {
		return 0, fmt.Errorf("supervisor: queue clear %s: %w", id, err)
	}
	m.mu.Lock()
	n := len(m.queue)
	m.queue = nil
	m.mu.Unlock()
	if n > 0 {
		s.notify()
	}
	return n, nil
}

// Interrupt cancels id's in-flight turn, if any. The session stays live and
// returns to idle; any queued prompts are untouched and dispatch normally
// afterward. It is a no-op on an idle session.
func (s *Supervisor) Interrupt(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, err := s.lookup(sessionID)
	if err != nil {
		return fmt.Errorf("supervisor: interrupt %s: %w", sessionID, err)
	}
	m.mu.Lock()
	cancel := m.turnCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Kill interrupts any in-flight turn, drops id from the roster, emits
// session.killed on its stream, and closes it. The on-disk journal is never
// deleted.
func (s *Supervisor) Kill(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, err := s.take(sessionID)
	if err != nil {
		return fmt.Errorf("supervisor: kill %s: %w", sessionID, err)
	}
	m.stop()
	m.sess.Emit(event.NewSessionKilled(sessionID))
	err = m.sess.Close()
	s.notify()
	return err
}

// Archive drops a finished session from the roster and emits
// session.archived, keeping its journal. It rejects (returns [ErrRunning]) a
// session with a turn in flight OR queued-but-not-yet-dispatched prompts —
// both surface as StatusWorking in the roster, and archiving a queued session
// would silently discard that pending work. Interrupt or kill it first.
//
// The check-then-act race between "is id idle and unqueued" and "remove id
// from the roster" is closed by holding both the roster lock and m's own lock
// across the whole decision: the state/queue check and setting m.closing
// happen under m.mu without releasing it, and m's pump goroutine only ever
// starts a new
// turn (transitioning idle -> running) while holding that same lock (see
// managed.pump). So whichever of {Archive's decision, the pump's next
// dispatch} acquires m.mu first is the one that happens — the pump can never
// slip a new turn in between Archive's idle check and its removal of id from
// the roster, and Archive can never observe idle for a session the pump has
// already committed to running.
func (s *Supervisor) Archive(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: archive %s: %w", sessionID, ErrClosed)
	}
	m, ok := s.roster[sessionID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: archive %s: %w", sessionID, ErrNotLive)
	}

	m.mu.Lock()
	if m.state == stateRunning || len(m.queue) > 0 {
		m.mu.Unlock()
		s.mu.Unlock()
		return fmt.Errorf("supervisor: archive %s: %w", sessionID, ErrRunning)
	}
	m.closing = true
	m.mu.Unlock()
	delete(s.roster, sessionID)
	s.mu.Unlock()

	m.stop()
	m.sess.Emit(event.NewSessionArchived(sessionID))
	err := m.sess.Close()
	s.notify()
	return err
}

// Roster returns a snapshot of live sessions, newest-first (by Created, then
// id, to keep ordering deterministic when timestamps tie).
func (s *Supervisor) Roster(ctx context.Context) ([]SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.snapshotLive(), nil
}

// snapshotLive builds the newest-first live-roster snapshot shared by Roster
// and the WatchRoster fan-out. It takes s.mu only to copy the managed
// pointers, then reads each session's info outside the roster lock.
func (s *Supervisor) snapshotLive() []SessionInfo {
	s.mu.Lock()
	ms := make([]*managed, 0, len(s.roster))
	for _, m := range s.roster {
		ms = append(ms, m)
	}
	s.mu.Unlock()

	out := make([]SessionInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.info())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// List enumerates every session on disk under the store root — live and
// archived/offline alike — overlaying live state from the roster. It walks
// <root>/sessions/<slug> directories directly and lists each via the shared
// store, since the SDK exposes no store-wide enumeration. Live entries carry
// full snapshot data (Status, Cost, ...); disk-only entries carry Live=false
// and a zero-value Status.
func (s *Supervisor) List(ctx context.Context) ([]SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sessionsDir := filepath.Join(s.root, "sessions")
	des, err := os.ReadDir(sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("supervisor: list %s: %w", sessionsDir, err)
	}

	live := s.liveByID()

	var out []SessionInfo
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		slug := de.Name()
		ids, err := s.store.List(ctx, slug)
		if err != nil {
			return nil, fmt.Errorf("supervisor: list project %s: %w", slug, err)
		}
		for _, id := range ids {
			if info, ok := live[id]; ok {
				out = append(out, info)
				continue
			}
			out = append(out, SessionInfo{
				ID:          id,
				Project:     slug,
				JournalPath: filepath.Join(sessionsDir, slug, id+".jsonl"),
				Live:        false,
			})
		}
	}
	return out, nil
}

// Subscribe returns a live event subscription for id. Errors with
// [ErrNotLive] if the session is not live.
func (s *Supervisor) Subscribe(ctx context.Context, sessionID string) (*event.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := s.lookup(sessionID)
	if err != nil {
		return nil, fmt.Errorf("supervisor: subscribe %s: %w", sessionID, err)
	}
	return m.sess.Events(), nil
}

// LastError returns id's most recent turn's Prompt error, if any — a
// best-effort diagnostic; the pump never treats a Prompt error as a
// supervisor-level failure (see managed.lastErr). It returns nil for an
// unknown or errorless id — use [Supervisor.Roster] to distinguish "unknown
// id" from "known and healthy" when that matters.
func (s *Supervisor) LastError(id string) error {
	m, err := s.lookup(id)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Close kills every live session (emitting session.killed for each, per the
// must-deliver contract), stops every WatchRoster subscriber, and closes the
// store the supervisor built itself (an injected [Config.Store] is left to
// its owner). Idempotent.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	all := make([]*managed, 0, len(s.roster))
	for _, m := range s.roster {
		all = append(all, m)
	}
	s.roster = make(map[string]*managed)
	s.mu.Unlock()

	// Wake and join every watcher goroutine so Close returns leak-free.
	s.watchMu.Lock()
	if !s.watchClosed {
		s.watchClosed = true
		close(s.watchDone)
	}
	s.watchMu.Unlock()
	s.watchWG.Wait()

	var errs []error
	for _, m := range all {
		m.stop()
		m.sess.Emit(event.NewSessionKilled(m.id))
		if err := m.sess.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.ownsStore {
		if err := s.store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// take removes id from the roster and returns it, or an error if it is not
// there (or the supervisor is closed). Removing under the roster lock, before
// stopping the session, ensures no concurrent Send/Interrupt/Archive call
// can find it again once Kill has claimed it.
func (s *Supervisor) take(id string) (*managed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	m, ok := s.roster[id]
	if !ok {
		return nil, ErrNotLive
	}
	delete(s.roster, id)
	return m, nil
}

// lookup returns id's managed session without removing it from the roster.
func (s *Supervisor) lookup(id string) (*managed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	m, ok := s.roster[id]
	if !ok {
		return nil, ErrNotLive
	}
	return m, nil
}

// get returns id's managed session and whether it is live (no closed check —
// Resume calls it to short-circuit an already-live id).
func (s *Supervisor) get(id string) (*managed, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.roster[id]
	return m, ok
}

// liveByID snapshots the roster into a map for List's overlay.
func (s *Supervisor) liveByID() map[string]SessionInfo {
	s.mu.Lock()
	ms := make([]*managed, 0, len(s.roster))
	for _, m := range s.roster {
		ms = append(ms, m)
	}
	s.mu.Unlock()

	out := make(map[string]SessionInfo, len(ms))
	for _, m := range ms {
		out[m.id] = m.info()
	}
	return out
}

// isClosed reports whether the supervisor has been closed.
func (s *Supervisor) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
