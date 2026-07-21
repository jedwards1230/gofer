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

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/permission"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/permrationale"
	"github.com/jedwards1230/gofer/internal/sandbox"
)

// Config configures a [Supervisor].
type Config struct {
	// Root is the shared session store's root directory. Empty resolves to
	// gofer's own default (~/.gofer) via [ResolveRoot] — gofer, not the SDK,
	// owns that default.
	Root string
	// Clock overrides the wall clock used to timestamp roster entries.
	// Defaults to time.Now. Test seam.
	Clock func() time.Time

	// MaxSubagentDepth caps how deep a subagent session tree may nest (see
	// [CreateOptions.ParentID]): a root session is depth 0 and a Create whose
	// parent already sits at this depth is refused with [ErrDepthExceeded]. Zero
	// or negative resolves to [config.DefaultMaxSubagentDepth] — zero is "unset",
	// never "no children allowed" — so a supervisor built without one behaves
	// like the operator default. Callers that load gofer's config pass
	// [config.Session.SubagentDepthLimit].
	MaxSubagentDepth int

	// Permissions returns a FRESH permission engine for each new session's guard
	// (see [config.Config.Engine]). A per-session engine keeps a remember-grant
	// — an allow rule appended by [loop.RuleGuard.Grant] when a human answers
	// "allow, remember" — scoped to the session that approved it, rather than
	// leaking that grant into every other live session over one shared engine.
	// Nil defaults to a permissive catch-all factory (allow → contain-or-ask),
	// matching an empty config — so a supervisor built without an explicit
	// policy never runs a tool uncontained without asking.
	Permissions func() *permission.Engine

	// Store, when set, is used instead of building a store from Root, and is
	// NOT closed by [Supervisor.Close] — the caller owns its lifecycle. Test
	// seam.
	Store *session.FileStore
	// NewSession, when set, replaces the default construction of a fresh
	// session (which calls [runner.New] with the shared store
	// injected). Test seam.
	NewSession func(ctx context.Context, opts runner.Options) (Session, error)
	// ResumeSession, when set, replaces the default reopening of an existing
	// session (which calls [runner.Resume] with the shared store injected).
	// Test seam.
	ResumeSession func(ctx context.Context, id string, opts runner.Options) (Session, error)

	// OnRegister, if set, is invoked once per session at registration —
	// before the session becomes reachable via the roster and before its
	// first turn can run — with the live session. It returns an optional
	// teardown func, joined (if non-nil) when the session stops; a nil
	// return means no teardown is needed. This is the supervisor's only hook
	// for attaching a per-session observer (e.g. telemetry) to a session's
	// event stream — the supervisor itself stays agnostic to what the
	// observer does with it. Nil is fine: the supervisor is fully
	// buildable/testable without one.
	OnRegister func(sess Session) (stop func())
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

	// maxSubagentDepth is the resolved [Config.MaxSubagentDepth] (never <= 0
	// after New).
	maxSubagentDepth int

	// onRegister mirrors Config.OnRegister; nil is fine (see its doc).
	onRegister func(sess Session) (stop func())

	// newEngine builds a fresh permission ruleset for each session's per-session
	// RuleGuard, so a remember-grant stays scoped to the approving session
	// (never nil after New — a nil Config.Permissions resolves to the default
	// contain-or-ask catch-all factory).
	newEngine func() *permission.Engine

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
	root, err := ResolveRoot(cfg.Root)
	if err != nil {
		return nil, err
	}

	store := cfg.Store
	ownsStore := false
	if store == nil {
		store, err = session.NewFileStore(session.WithRoot(root))
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
			return runner.New(ctx, opts)
		}
	}
	resumeSession := cfg.ResumeSession
	if resumeSession == nil {
		resumeSession = func(ctx context.Context, id string, opts runner.Options) (Session, error) {
			opts.Store = store
			return runner.Resume(ctx, id, opts)
		}
	}

	newEngine := cfg.Permissions
	if newEngine == nil {
		// No explicit policy: default to a catch-all allow so an unmatched call
		// resolves to allow → the RuleGuard consults the sandbox Container
		// (contain-or-ask), never running a tool uncontained. Mirrors
		// config.Config{}.Engine(). A fresh engine per session keeps grants
		// session-scoped.
		newEngine = func() *permission.Engine {
			return permission.New(permission.Rule{
				Verdict:   event.VerdictAllow,
				Tool:      "*",
				Specifier: "*",
				Source:    "default",
			})
		}
	}

	maxDepth := cfg.MaxSubagentDepth
	if maxDepth <= 0 {
		maxDepth = config.DefaultMaxSubagentDepth
	}

	return &Supervisor{
		root:             root,
		store:            store,
		ownsStore:        ownsStore,
		clock:            clock,
		newSession:       newSession,
		resumeSession:    resumeSession,
		maxSubagentDepth: maxDepth,
		onRegister:       cfg.OnRegister,
		newEngine:        newEngine,
		roster:           make(map[string]*managed),
		watchers:         make(map[*watcher]struct{}),
		watchDone:        make(chan struct{}),
	}, nil
}

// sessionGuard builds the per-session permission plumbing: a fresh reply Gate
// (reply routing is per-session — see [Supervisor.Reply]), a sandbox Container
// shared between the RuleGuard's containability check and the sandbox-wrapping
// tool registry, and the compiled RuleGuard over the shared engine. It returns
// the three runner.Options fields to inject plus the Gate to store on the
// managed session.
func (s *Supervisor) sessionGuard(cwd string) (guard loop.Guard, approver *loop.Gate, tools loop.ToolRegistry) {
	gate := loop.NewGate()
	container := sandbox.New()
	rg := loop.RuleGuard{Engine: s.newEngine(), Container: container, Target: sandbox.ToolTarget}
	return rg, gate, sandbox.WrapRegistry(cwd, container)
}

// ResolveRoot is gofer's single source of the ~/.gofer default — the SDK
// invents no directory name of its own, so every store/auth/runner
// construction in this binary must resolve an empty root through here before
// it reaches the SDK. It also lets the supervisor enumerate <root>/sessions
// itself in List — the SDK exposes no store-wide "list every session" call.
// Exported so internal/daemon can resolve the same default when locating the
// endpoint file it advertises alongside the session store (see
// internal/daemon.EndpointPath) — an empty root always means the same
// directory to every part of gofer, never re-derived independently.
func ResolveRoot(root string) (string, error) {
	if root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("supervisor: resolve default store root: %w", err)
	}
	return filepath.Join(home, ".gofer"), nil
}

// CreateOptions configures [Supervisor.Create]. Model is REQUIRED — the
// supervisor resolves no default of its own, and an empty one is rejected
// with [ErrNoModel] rather than passed down to the runner. An empty Cwd
// resolves to the daemon's working directory (the caller's responsibility
// upstream) and a zero MaxIters to the loop default.
type CreateOptions struct {
	Model    string
	Cwd      string
	System   string
	Params   provider.Params
	MaxIters int

	// ParentID is the id of the session SPAWNING this one — empty for a root
	// session, which is every session gofer created before subagents existed.
	// When set, the new session is a real child session (its own journal, cost
	// and transcript) linked to that parent: Create resolves the parent (live
	// roster first, then the store root on disk), derives Depth = parent+1,
	// enforces [Config.MaxSubagentDepth], and persists the link beside the
	// journal so it survives a restart. An unknown parent is [ErrNoParent]; too
	// deep is [ErrDepthExceeded].
	//
	// The link is gofer-native on purpose: supervision and roster stay above the
	// SDK (the SDK promotion test), and the SDK has no session-parent concept.
	ParentID string

	// Agent is the child's agent type/identity (e.g. "go-developer"). It is
	// forwarded verbatim to [runner.Options.Agent] — the one SDK surface this
	// feature consumes — which stamps it onto every event.ToolCallStarted/
	// Delta/Finished the session's loop emits, so a transcript interleaving a
	// parent's and its children's tool calls can attribute each one. Empty is
	// un-attributed, the default and the pre-existing behavior.
	//
	// It is independent of ParentID: a root session may carry an agent identity,
	// and a child need not.
	Agent string
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
	// Fail here, with the remedy named, rather than letting an empty model id
	// reach the SDK and come back as `runner: unknown model ""` — see
	// [ErrNoModel]. Every caller resolves a model before this point; reaching
	// it empty means that resolution found nothing, which is exactly what the
	// operator needs told.
	if opts.Model == "" {
		return SessionInfo{}, fmt.Errorf("supervisor: create session: %w — run 'gofer login <provider>' if none is logged in, or set session.model in %s",
			ErrNoModel, filepath.Join(s.root, config.ConfigFileName))
	}
	meta, err := s.resolveParent(opts)
	if err != nil {
		return SessionInfo{}, err
	}

	guard, gate, tools := s.sessionGuard(opts.Cwd)
	sess, err := s.newSession(ctx, runner.Options{
		Root: s.root, Cwd: opts.Cwd, Model: opts.Model, System: opts.System,
		Params: opts.Params, MaxIters: opts.MaxIters, Agent: opts.Agent,
		Guard: guard, Approver: gate, Tools: tools,
	})
	if err != nil {
		return SessionInfo{}, fmt.Errorf("supervisor: create session: %w", err)
	}

	// The runner seeds its own effort from Params.Thinking.Effort at
	// construction, so the roster's bookkeeping starts from the same value
	// rather than from a hardcoded "" that a later SetEffort would silently
	// contradict (see managed.effort).
	// Persist the parent/agent link beside the journal BEFORE the session becomes
	// reachable, so a client that sees the session at all sees a linked one. A
	// plain root session records nothing (see sessionMeta.recordable), which is
	// why this whole path is inert for every pre-existing caller. A failed write
	// fails the create: a child whose link was silently dropped is an orphan the
	// roster can never place in its tree, which is worse than a loud refusal.
	if meta.recordable() {
		if err := writeSessionMeta(filepath.Dir(sess.JournalPath()), sess.ID(), meta); err != nil {
			_ = sess.Close()
			return SessionInfo{}, fmt.Errorf("supervisor: create session: %w", err)
		}
	}

	m, err := s.register(sess, opts.Model, opts.Params.Thinking.Effort, opts.Cwd, gate, meta)
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

// resolveParent turns opts' subagent fields into the [sessionMeta] the new
// session is registered and (when there is anything to record) journaled
// alongside with.
//
// With no ParentID the result is a root session at depth 0 — carrying opts.Agent
// if one was given, since agent attribution is independent of the parent link.
// With one, the parent is resolved LIVE FIRST (its depth is authoritative and
// in memory) and only then from disk, so a parent whose process was restarted —
// or one owned by a different worker over the same shared store, which is the
// normal case under M6 process isolation — still resolves. A parent found on
// disk with no sidecar is a root at depth 0, which is exactly what it is.
//
// Write-ordering note, for whoever later makes Create cheaper: the sidecar is
// written BEFORE the session becomes reachable (see Create), and that ordering
// is load-bearing HERE. Reverse it — register first, persist later — and a
// window opens in which a parent's journal exists but its sidecar does not, so
// this lookup reads the parent back as a root at depth 0 and a child spawned in
// that window silently dodges the cap. The window is unreachable today (a
// client cannot know a session's id before Create returns it, and Create
// returns only once the sidecar has landed); keep it that way.
func (s *Supervisor) resolveParent(opts CreateOptions) (sessionMeta, error) {
	meta := sessionMeta{ParentID: opts.ParentID, Agent: opts.Agent}
	if opts.ParentID == "" {
		return meta, nil
	}

	var parentDepth int
	if m, ok := s.get(opts.ParentID); ok {
		parentDepth = m.depth
	} else if pm, found := lookupDiskSession(s.root, opts.ParentID); found {
		parentDepth = pm.Depth
	} else {
		return sessionMeta{}, fmt.Errorf("supervisor: create session: %w: %s", ErrNoParent, opts.ParentID)
	}

	meta.Depth = parentDepth + 1
	if meta.Depth > s.maxSubagentDepth {
		return sessionMeta{}, fmt.Errorf("supervisor: create session: %w: depth %d exceeds the cap of %d — raise session.max_subagent_depth in %s",
			ErrDepthExceeded, meta.Depth, s.maxSubagentDepth, filepath.Join(s.root, config.ConfigFileName))
	}
	return meta, nil
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

	// A resumed child keeps its attribution: the parent link, agent id and depth
	// are read back from the on-disk sidecar (see [sessionMeta]) BEFORE the
	// runner is built, since Agent is a construction-time runner option. A
	// session with no sidecar — every session predating subagents — reads back
	// the zero value and resumes exactly as it always did.
	//
	// This scans rather than jumping straight to session.Slugify(opts.Cwd), and
	// that is NOT an oversight: a session's directory is the slug of the cwd it
	// was CREATED with, while opts.Cwd is the cwd this resume asks for, and ACP
	// makes the latter client-supplied and authoritative — the two legitimately
	// differ. The SDK's own store resolves a journal by id the same way
	// ([session.FileStore] scans the project dirs); shortcutting to the resume
	// cwd's slug would silently read no sidecar and drop the child's attribution
	// exactly when a client reopened it from somewhere else.
	meta, _ := lookupDiskSession(s.root, id)

	guard, gate, tools := s.sessionGuard(opts.Cwd)
	sess, err := s.resumeSession(ctx, id, runner.Options{
		Root: s.root, Cwd: opts.Cwd, Model: opts.Model, System: opts.System,
		Params: opts.Params, MaxIters: opts.MaxIters, Agent: meta.Agent,
		Guard: guard, Approver: gate, Tools: tools,
	})
	if err != nil {
		return SessionInfo{}, fmt.Errorf("supervisor: resume %s: %w", id, err)
	}

	m, err := s.register(sess, opts.Model, opts.Params.Thinking.Effort, opts.Cwd, gate, meta)
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
// only once the insert is committed, so a rejected registration leaks
// nothing. newManaged invokes onRegister (if set) while building m, before m
// is published into the roster below — so a concurrent Kill can never
// observe a session whose teardown hasn't been stashed yet (see
// Config.OnRegister's doc).
func (s *Supervisor) register(sess Session, model, effort, cwd string, gate *loop.Gate, meta sessionMeta) (*managed, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	m := newManaged(sess, model, effort, s.clock(), s.clock, s.notify, cwd, gate, meta, s.onRegister)
	s.roster[m.id] = m
	s.mu.Unlock()

	go m.pump()
	// Subscribe to the session's own stream to keep the live pending-approval
	// count (see managed.watchPermissions). Subscribed here, at registration,
	// before any turn can run — so the count never misses this session's first
	// permission request.
	go m.watchPermissions(sess.Events())
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

// SetModel changes sessionID's model for its next turn, consuming the SDK
// runner's own setter ([runner.Runner.SetModel]) — no journal rebuild is
// needed, since the swap is same-provider and takes effect on the session's
// next turn (see that method's doc). Unlike Archive, SetModel has no
// idle-only restriction: the SDK setter is concurrency-safe with a turn
// already in flight (it reads the model once, at the top of the NEXT turn),
// so calling it on a running session is fine.
//
// Before calling into the SDK, SetModel does its own cross-provider
// pre-check via [provider.Lookup] so a rejection surfaces as the typed
// [ErrCrossProvider] sentinel — the SDK's own rejection (invoked below as
// defense-in-depth) is a plain, unwrapped error a caller cannot errors.Is
// against (see [runner.Runner.SetModel]'s doc). The pre-check compares the
// TARGET model's provider against the session's CURRENT model (as tracked in
// this package's own roster bookkeeping — the [Session] interface exposes no
// model accessor). If the current model is not a registered id (only
// possible for a non-registry/test model id — a session's model at creation
// is never itself validated against the registry), the provider comparison
// is skipped and the call defers entirely to the SDK's own validation.
func (s *Supervisor) SetModel(ctx context.Context, sessionID, model string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if model == "" {
		return fmt.Errorf("supervisor: set model %s: %w", sessionID, ErrEmptyModel)
	}
	m, err := s.lookup(sessionID)
	if err != nil {
		return fmt.Errorf("supervisor: set model %s: %w", sessionID, err)
	}

	next, err := provider.Resolve(model)
	if err != nil {
		return fmt.Errorf("supervisor: set model %s: %w", sessionID, err)
	}
	m.mu.Lock()
	current := m.model
	m.mu.Unlock()
	// Resolve for the CURRENT model too, not just the target. With Lookup here,
	// a session already running an unregistered-but-runnable id (say a model
	// newer than this binary's registry) would fail the ok check, silently
	// skipping the cross-provider guard entirely — so a genuine cross-provider
	// swap would slip past and come back from the SDK as a plain error the TUI
	// cannot errors.Is against, losing the ErrCrossProvider branch it needs.
	// Resolve infers the provider from the id's shape, so the guard keeps
	// firing. A current id Resolve CANNOT place (a non-registry test model)
	// still skips the comparison and defers to the SDK, exactly as before.
	if cur, cerr := provider.Resolve(current); cerr == nil && cur.Provider != next.Provider {
		return fmt.Errorf("supervisor: set model %s: cannot change from %q (%s) to %q (%s): %w",
			sessionID, current, cur.Provider, model, next.Provider, ErrCrossProvider)
	}

	if err := m.sess.SetModel(model); err != nil {
		return fmt.Errorf("supervisor: set model %s: %w", sessionID, err)
	}

	m.mu.Lock()
	m.model = model
	m.mu.Unlock()
	s.notify()
	return nil
}

// SetEffort changes sessionID's reasoning effort for its next turn, consuming
// the SDK runner's own setter ([runner.Runner.SetEffort]) — the effort-axis
// parallel to [Supervisor.SetModel], hop for hop. Like SetModel it has no
// idle-only restriction: the SDK setter is concurrency-safe with a turn already
// in flight (the runner reads the effort once, at the top of the NEXT turn).
//
// Two ways it deliberately DIFFERS from SetModel:
//
//   - An empty effort is LEGAL. "" is the SDK's documented "clear back to the
//     provider's default" ([provider.ValidEffort]), so there is no
//     ErrEmptyModel analogue; the pre-check is ValidEffort, which admits "".
//   - There is no cross-provider guard. A runner's provider client is fixed at
//     creation, which is what constrains a model swap — but effort is
//     provider-agnostic vocabulary each backend projects onto its own wire
//     format (OpenAI's effort string, Anthropic's thinking budget), so the same
//     level is meaningful everywhere and nothing about the session's provider
//     can make it illegal.
//
// The reasoning-CAPABILITY check is left entirely to the SDK: [runner.Runner.SetEffort]
// rejects a non-empty level only on positive registry evidence that the current
// model cannot reason, and duplicating that rule here would be a second copy to
// drift. Clients that want to refuse BEFORE calling (the TUI's picker does, so
// it never offers four levels the runner will reject) read the same
// [provider.Lookup] capability bit themselves.
func (s *Supervisor) SetEffort(ctx context.Context, sessionID, effort string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Pre-checked here, rather than left to the SDK, for the same reason
	// SetModel pre-checks cross-provider: the SDK's rejection is a plain,
	// unwrapped error a caller cannot errors.Is against, and the daemon wire
	// flattens it further. ValidEffort is the SDK's own predicate, so this is a
	// typed restatement of its verdict, not a second opinion.
	if !provider.ValidEffort(effort) {
		return fmt.Errorf("supervisor: set effort %s: %w: %q (want %q, %q, %q, or \"\" to clear)",
			sessionID, ErrInvalidEffort, effort, provider.EffortLow, provider.EffortMedium, provider.EffortHigh)
	}
	m, err := s.lookup(sessionID)
	if err != nil {
		return fmt.Errorf("supervisor: set effort %s: %w", sessionID, err)
	}

	if err := m.sess.SetEffort(effort); err != nil {
		return fmt.Errorf("supervisor: set effort %s: %w", sessionID, err)
	}

	m.mu.Lock()
	m.effort = effort
	m.mu.Unlock()
	s.notify()
	return nil
}

// EmitConfigOptions publishes a session.config event onto sessionID's own event
// stream via the runner Emit seam — the same seam the session-title emit uses
// (see managed.go's enqueue) — carrying options as the session's authoritative
// config-options snapshot. It is how the daemon advertises a model change to
// clients: the event projects to an ACP config_option_update (see
// acp.ToSessionUpdate). WHICH options exist and their values is the daemon's
// business knowledge, so the daemon builds the neutral snapshot and this method
// only carries it onto the stream (mirroring how title derivation lives above
// the SDK while the SDK carries the resulting event.SessionInfoUpdated).
// Returns [ErrNotLive] for an unknown or archived session.
//
// Emit is called without m.mu held, per this package's lock discipline (a
// must-deliver publish can block on broker backpressure — see managed.go's
// enqueue): lookup takes s.mu only to resolve the managed session, then releases
// it before the publish.
func (s *Supervisor) EmitConfigOptions(sessionID string, options []event.ConfigOption) error {
	m, err := s.lookup(sessionID)
	if err != nil {
		return fmt.Errorf("supervisor: emit config options %s: %w", sessionID, err)
	}
	m.sess.Emit(event.NewConfigOptionsUpdated(sessionID, options))
	return nil
}

// Reply routes a human's permission answer to sessionID's approval gate,
// unblocking the guard's Await for the matching call id (see [loop.Gate]). It
// errors with [ErrNotLive] for an unknown session. The reply carries no session
// id itself (see [event.PermissionReply]); the daemon resolves which session by
// the call id it recorded when it broadcast the request (see the daemon's
// permission-route map).
func (s *Supervisor) Reply(sessionID string, op event.PermissionReply) error {
	m, err := s.lookup(sessionID)
	if err != nil {
		return fmt.Errorf("supervisor: reply %s: %w", sessionID, err)
	}
	m.gate.Reply(op)
	return nil
}

// ExplainPermission answers why sessionID's still-pending callID was gated,
// deriving the rationale from the guard's own decision trace (see
// [permrationale.Derive] — the SAME grammar the daemon answers ACP's
// session/explain_permission with, so a TUI running against an in-process
// supervisor and one running against a daemon get the same explanation).
//
// It is READ-ONLY, and that is a contract, not an implementation detail: it
// never touches the gate, never resolves the request, and never mutates the
// retained payload. A client can ask as many times as it likes and the human
// still has to answer the prompt afterwards.
//
// It errors with [ErrNotLive] for an unknown session and
// [ErrNoPendingPermission] for a call id that is not outstanding on it —
// including one that belongs to a DIFFERENT session, since each session's
// retained requests are its own, so one session's rationale can never leak
// through another's id.
func (s *Supervisor) ExplainPermission(sessionID, callID string) (acp.PermissionRationale, error) {
	m, err := s.lookup(sessionID)
	if err != nil {
		return acp.PermissionRationale{}, fmt.Errorf("supervisor: explain permission %s: %w", sessionID, err)
	}
	pe, ok := m.pendingPerm(callID)
	if !ok {
		return acp.PermissionRationale{}, fmt.Errorf("supervisor: explain permission %s/%s: %w", sessionID, callID, ErrNoPendingPermission)
	}
	return permrationale.Derive(pe.Tool, pe.Trace), nil
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
// full snapshot data (Status, Cost, ...); disk-only entries carry Live=false,
// a zero-value Status, and are enriched from their journal (see
// [diskSessionInfo]) — Cwd, Title, and Updated all survive a process restart.
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
			path := filepath.Join(sessionsDir, slug, id+".jsonl")
			out = append(out, diskSessionInfo(id, slug, path))
		}
	}
	return out, nil
}

// diskSessionInfo builds a disk-only [SessionInfo] for id under slug at path,
// enriched from the journal read-only via [session.ReadEntries] (no append
// handle is opened — List never resumes a session just to enumerate it):
//
//   - Cwd comes from the journal's [session.EntryMeta] root entry (see
//     [session.NewMetaEntry]), always its first entry when present. A legacy
//     journal written before the SDK started persisting cwd has no meta
//     entry, so Cwd is left "" — the caller falls back to some other default
//     (see [handleSessionList]'s daemonCwd fallback in package daemon).
//   - Title mirrors how a live session derives it (see managed.go's
//     enqueue/snippet): an excerpt of the first user-role message's text.
//   - Created and Updated come from the first and last entry's Time.
//   - ParentID, Agent and Depth come from the session's `<id>.meta.json`
//     sidecar (see [sessionMeta]), so an offline child still shows its parent
//     link. A session with no sidecar — every session predating subagents, and
//     every root session, which writes none — reads back zero values and lists
//     exactly as it did before.
//
// A read error, or a journal with no entries at all, degrades to the bare
// {ID, Project, JournalPath, Live:false} snapshot rather than failing the
// whole List — one unreadable journal must never hide every other session.
func diskSessionInfo(id, slug, path string) SessionInfo {
	meta := readSessionMeta(sidecarPath(filepath.Dir(path), id))
	info := SessionInfo{
		ID:          id,
		Project:     slug,
		JournalPath: path,
		Live:        false,
		ParentID:    meta.ParentID,
		Agent:       meta.Agent,
		Depth:       meta.Depth,
	}

	entries, err := session.ReadEntries(path)
	if err != nil || len(entries) == 0 {
		return info
	}

	info.Created = entries[0].Time
	info.Updated = entries[len(entries)-1].Time

	if entries[0].Type == session.EntryMeta {
		if meta, metaErr := entries[0].Meta(); metaErr == nil {
			info.Cwd = meta.Cwd
		}
	}

	for _, e := range entries {
		if e.Type != session.EntryMessage {
			continue
		}
		msg, msgErr := e.Message()
		if msgErr != nil || msg.Role != provider.RoleUser {
			continue
		}
		if text := msg.Text(); text != "" {
			info.Title = snippet(text)
			break
		}
	}

	return info
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

// SubscribeLive is [Supervisor.Subscribe] without the retained must-deliver
// backlog replay — for a caller driving a new turn that must not observe a
// prior turn's retained terminal event (see [Session.EventsLive]). Subscribe
// (with replay) stays the right call for attach/peek, where recovering
// missed events is the point.
func (s *Supervisor) SubscribeLive(ctx context.Context, sessionID string) (*event.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := s.lookup(sessionID)
	if err != nil {
		return nil, fmt.Errorf("supervisor: subscribe %s: %w", sessionID, err)
	}
	return m.sess.EventsLive(), nil
}

// History returns id's folded conversation history as provider messages —
// the same settled-journal snapshot [Supervisor.Subscribe]'s live stream
// builds on. It errors with [ErrNotLive] if the session is not live (M2:
// history is only readable while a session is registered, mirroring
// Subscribe).
func (s *Supervisor) History(ctx context.Context, id string) ([]provider.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := s.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("supervisor: history %s: %w", id, err)
	}
	return m.sess.Fold(), nil
}

// AwaitSettled blocks until id is safe for a complete history fold, then
// returns nil: it is idle (reporting [StatusNeedsInput]), it has left the
// roster, or it was never live here. It returns the ctx error when ctx is
// cancelled or its deadline fires first. It is the fix for issue #137: a turn's
// assistant/tool entries are journaled ASYNCHRONOUSLY by the runner's consume
// goroutine AFTER the turn.finished event a client observes, so a session/load
// landing in that window reads — and silently replays — a SHORT history. The
// pump publishes [StatusNeedsInput] only after Session.Prompt returns, and
// Prompt returns only after the runner's awaitJournaled barrier, so needs-input
// is the locally-observable proof that the fold is whole (the same inference
// awaitFoldComplete relies on in tests).
//
// It is deliberately BEST-EFFORT and unbounded of its own: the caller
// (handleSessionLoad) applies the timeout bound via ctx, and a ctx deadline
// returned here is the caller's cue to "fold whatever is durable" rather than a
// failure — so a session genuinely mid-turn, which never reaches needs-input,
// cannot deadlock the load. A not-live/offline id returns nil at once: its
// on-disk journal has no live writer, so it is already complete.
//
// It observes the settle signal on the same [Supervisor.WatchRoster] stream the
// roster fan-out already maintains — not a poll — so it wakes on the exact
// idle⇄running transition that means the turn is journaled.
func (s *Supervisor) AwaitSettled(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Not live here → offline; the on-disk journal has no live writer and is
	// already durable, so there is nothing to wait for.
	if _, err := s.lookup(id); err != nil {
		return nil //nolint:nilerr // not-live/closed is "nothing to settle", not a failure
	}

	roster, err := s.WatchRoster(ctx)
	if err != nil {
		// Closed between the lookup and the watch — nothing left to settle.
		return nil //nolint:nilerr // see above
	}
	for {
		select {
		case snap, ok := <-roster:
			if !ok {
				// The watch closed (the supervisor is shutting down): stop waiting
				// rather than block, and let the caller fold what is durable.
				return nil
			}
			settled, present := settledInRoster(snap, id)
			if settled || !present {
				// present=false means id left the roster (killed/archived) — no live
				// writer remains, so its journal is complete.
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// settledInRoster reports whether snap holds id and, if so, whether it is idle
// (reporting [StatusNeedsInput]). present=false means id is not in the snapshot.
func settledInRoster(snap []SessionInfo, id string) (settled, present bool) {
	for _, si := range snap {
		if si.ID == id {
			return si.Status == StatusNeedsInput, true
		}
	}
	return false, false
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
