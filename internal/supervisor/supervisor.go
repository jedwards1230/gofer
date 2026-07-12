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

// CreateOptions configures [Supervisor.Create].
type CreateOptions struct {
	Cwd, Model, System string
	Params             provider.Params
	MaxIters           int
}

// Create starts a fresh session and registers it live (state [StateIdle]).
func (s *Supervisor) Create(ctx context.Context, opts CreateOptions) (RosterEntry, error) {
	if s.isClosed() {
		return RosterEntry{}, ErrClosed
	}
	sess, err := s.newSession(ctx, runner.Options{
		Root: s.root, Cwd: opts.Cwd, Model: opts.Model, System: opts.System,
		Params: opts.Params, MaxIters: opts.MaxIters,
	})
	if err != nil {
		return RosterEntry{}, fmt.Errorf("supervisor: create session: %w", err)
	}
	return s.register(sess, opts.Model), nil
}

// ResumeOptions configures [Supervisor.Resume]. Model and Cwd are required —
// the journal itself does not persist them.
type ResumeOptions struct {
	Cwd, Model, System string
	Params             provider.Params
	MaxIters           int
}

// Resume reopens an on-disk session and registers it live. If id is already
// live, Resume is a no-op that returns the existing roster entry — it never
// builds a second runner over the same journal.
func (s *Supervisor) Resume(ctx context.Context, id string, opts ResumeOptions) (RosterEntry, error) {
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()

	if entry, ok := s.liveEntry(id); ok {
		return entry, nil
	}
	if s.isClosed() {
		return RosterEntry{}, ErrClosed
	}

	sess, err := s.resumeSession(ctx, id, runner.Options{
		Root: s.root, Cwd: opts.Cwd, Model: opts.Model, System: opts.System,
		Params: opts.Params, MaxIters: opts.MaxIters,
	})
	if err != nil {
		return RosterEntry{}, fmt.Errorf("supervisor: resume %s: %w", id, err)
	}
	return s.register(sess, opts.Model), nil
}

// register adds sess to the roster as a live, idle session and starts its
// pump goroutine.
func (s *Supervisor) register(sess Session, model string) RosterEntry {
	m := newManaged(sess, model, s.clock())

	s.mu.Lock()
	s.roster[m.id] = m
	s.mu.Unlock()

	go m.pump()
	return m.entry()
}

// Submit enqueues a prompt for id. It dispatches immediately when the
// session is idle, else queues FIFO. The returned position is 0-based: 0
// means the prompt will run next (idle) or is itself already running is
// never returned here (Submit only ever enqueues) — 0 means it is about to
// be the next dispatched, and a session with a turn already in flight
// returns 1 for the first prompt queued behind it, 2 for the second, and so
// on, treating the running turn as occupying position 0.
func (s *Supervisor) Submit(id, text string) (int, error) {
	m, err := s.lookup(id)
	if err != nil {
		return 0, fmt.Errorf("supervisor: submit %s: %w", id, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return 0, fmt.Errorf("supervisor: submit %s: %w", id, ErrNotLive)
	}
	m.queue = append(m.queue, text)
	pos := len(m.queue) - 1
	if m.state == StateRunning {
		pos++
	}

	select {
	case m.submitCh <- struct{}{}:
	default:
	}
	return pos, nil
}

// QueueList returns the pending (not yet dispatched) prompt texts for id, in
// FIFO order.
func (s *Supervisor) QueueList(id string) ([]string, error) {
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
func (s *Supervisor) QueueClear(id string) (int, error) {
	m, err := s.lookup(id)
	if err != nil {
		return 0, fmt.Errorf("supervisor: queue clear %s: %w", id, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.queue)
	m.queue = nil
	return n, nil
}

// Interrupt cancels id's in-flight turn, if any. The session stays live and
// returns to [StateIdle]; any queued prompts are untouched and dispatch
// normally afterward. It is a no-op on an idle session.
func (s *Supervisor) Interrupt(id string) error {
	m, err := s.lookup(id)
	if err != nil {
		return fmt.Errorf("supervisor: interrupt %s: %w", id, err)
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
func (s *Supervisor) Kill(id string) error {
	m, err := s.take(id)
	if err != nil {
		return fmt.Errorf("supervisor: kill %s: %w", id, err)
	}
	m.stop()
	m.sess.Emit(event.NewSessionKilled(id))
	return m.sess.Close()
}

// Archive drops a finished session from the roster and emits
// session.archived, keeping its journal. It rejects (returns [ErrRunning])
// a session with a turn in flight — kill it first.
//
// The check-then-act race between "is id idle" and "remove id from the
// roster" is closed by holding both the roster lock and m's own lock across
// the whole decision: the state check and setting m.closing happen under
// m.mu without releasing it, and m's pump goroutine only ever starts a new
// turn (transitioning idle -> running) while holding that same lock (see
// managed.pump). So whichever of {Archive's decision, the pump's next
// dispatch} acquires m.mu first is the one that happens — the pump can never
// slip a new turn in between Archive's idle check and its removal of id from
// the roster, and Archive can never observe idle for a session the pump has
// already committed to running.
func (s *Supervisor) Archive(id string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: archive %s: %w", id, ErrClosed)
	}
	m, ok := s.roster[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: archive %s: %w", id, ErrNotLive)
	}

	m.mu.Lock()
	if m.state == StateRunning {
		m.mu.Unlock()
		s.mu.Unlock()
		return fmt.Errorf("supervisor: archive %s: %w", id, ErrRunning)
	}
	m.closing = true
	m.mu.Unlock()
	delete(s.roster, id)
	s.mu.Unlock()

	m.stop()
	m.sess.Emit(event.NewSessionArchived(id))
	return m.sess.Close()
}

// stop marks m closing, cancels its base context (interrupting any in-flight
// turn and waking an idle pump), and waits for its pump goroutine to exit.
func (m *managed) stop() {
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
	m.baseCancel()
	<-m.done
}

// Roster returns a snapshot of live sessions, newest-first (by CreatedAt,
// then id, to keep ordering deterministic when timestamps tie).
func (s *Supervisor) Roster() []RosterEntry {
	s.mu.Lock()
	entries := make([]RosterEntry, 0, len(s.roster))
	for _, m := range s.roster {
		entries = append(entries, m.entry())
	}
	s.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		}
		return entries[i].ID > entries[j].ID
	})
	return entries
}

// List enumerates every session on disk under the store root — live and
// archived/offline alike — overlaying live state from the roster. It walks
// <root>/sessions/<slug> directories directly and lists each via the shared
// store, since the SDK exposes no store-wide enumeration.
func (s *Supervisor) List(ctx context.Context) ([]SessionInfo, error) {
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
			info := SessionInfo{
				ID:          id,
				Project:     slug,
				JournalPath: filepath.Join(sessionsDir, slug, id+".jsonl"),
			}
			if entry, ok := live[id]; ok {
				info.Live = true
				info.State = entry.State
			}
			out = append(out, info)
		}
	}
	return out, nil
}

// Subscribe returns a live event subscription for id. Errors with
// [ErrNotLive] if the session is not live.
func (s *Supervisor) Subscribe(id string) (*event.Subscription, error) {
	m, err := s.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("supervisor: subscribe %s: %w", id, err)
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
// must-deliver contract) and closes the store the supervisor built itself
// (an injected [Config.Store] is left to its owner). Idempotent.
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
// stopping the session, ensures no concurrent Submit/Interrupt/Archive call
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

// liveEntry returns id's roster entry and true if it is live.
func (s *Supervisor) liveEntry(id string) (RosterEntry, bool) {
	s.mu.Lock()
	m, ok := s.roster[id]
	s.mu.Unlock()
	if !ok {
		return RosterEntry{}, false
	}
	return m.entry(), true
}

// liveByID snapshots the roster into a map for List's overlay.
func (s *Supervisor) liveByID() map[string]RosterEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]RosterEntry, len(s.roster))
	for id, m := range s.roster {
		out[id] = m.entry()
	}
	return out
}

// isClosed reports whether the supervisor has been closed.
func (s *Supervisor) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
