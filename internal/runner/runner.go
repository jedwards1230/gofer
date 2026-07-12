// Package runner is gofer's session glue: it builds a real provider and tool
// registry, drives the SDK's agent loop, and streams the loop's typed events
// into a durable session journal as each model-call turn settles. The SDK
// drives the loop and emits events; it does not persist anything. That
// persistence — and folding a journal back into provider messages on resume —
// is gofer-owned, consuming the SDK only through its exported
// provider/auth/tool/loop/event/session APIs (never SDK internals).
package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"
	"github.com/jedwards1230/agent-sdk-go/tool"
)

// defaultSubBuffer is the channel buffer for the Runner's own event
// subscriptions — ample for one interactive session.
const defaultSubBuffer = 256

// defaultReplay is how many must-deliver events the broker retains so a
// subscriber attaching after construction still receives session.created /
// session.resumed.
const defaultReplay = 256

// Options configures a Runner. Model, Cwd, and (for a fresh session) Root are
// the only fields a caller normally sets; Provider, Tools, IDGen, and Clock
// are test seams.
type Options struct {
	// Root is the session store's root directory (holds sessions/ and, for a
	// real provider, auth.json). Empty uses the SDK default (~/.gofer).
	Root string
	// Cwd is the working directory tools operate in, and the project the
	// session belongs to (via session.Slugify).
	Cwd string
	// Model is the model identifier passed to the provider and loop.
	Model string
	// System is the system prompt.
	System string
	// Params carries sampling and reasoning controls.
	Params provider.Params
	// MaxIters caps model-call rounds per Prompt; <= 0 uses the loop default.
	MaxIters int

	// IDGen overrides the session/entry id generator. Test seam.
	IDGen func() string
	// Clock overrides the wall clock used to timestamp journal entries. Test
	// seam.
	Clock func() time.Time

	// Provider, when set, is used instead of building a real provider from
	// Model via auth + provider.Lookup. Test seam.
	Provider provider.Provider
	// Tools, when set, is used instead of the builtin tool set rooted at Cwd.
	// Test seam.
	Tools loop.ToolRegistry
}

// Runner drives one session: it owns the provider, tool registry, event
// broker, and session journal, and folds the journal back into provider
// messages so a Prompt after Resume continues with full prior context.
type Runner struct {
	model    string
	system   string
	params   provider.Params
	maxIters int

	provider provider.Provider
	tools    loop.ToolRegistry

	broker  *event.Broker
	journal *session.Journal
	store   *session.FileStore

	journalDone chan struct{}

	mu   sync.Mutex
	jerr error
}

// NewSession builds a Runner around a freshly created journal for the
// project at opts.Cwd.
func NewSession(ctx context.Context, opts Options) (*Runner, error) {
	store, err := newStore(opts)
	if err != nil {
		return nil, err
	}
	slug := session.Slugify(opts.Cwd)
	journal, err := store.Create(ctx, slug)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("runner: create session: %w", err)
	}
	return build(opts, store, journal, false)
}

// Resume builds a Runner around the existing journal for id, publishing
// session.resumed once the runner is live.
func Resume(ctx context.Context, id string, opts Options) (*Runner, error) {
	store, err := newStore(opts)
	if err != nil {
		return nil, err
	}
	journal, err := store.Open(ctx, id)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("runner: open session %s: %w", id, err)
	}
	return build(opts, store, journal, true)
}

// newStore builds the journal store from opts, wiring the deterministic id
// generator / clock test seams when set.
func newStore(opts Options) (*session.FileStore, error) {
	var storeOpts []session.StoreOption
	if opts.Root != "" {
		storeOpts = append(storeOpts, session.WithRoot(opts.Root))
	}
	if opts.IDGen != nil {
		storeOpts = append(storeOpts, session.WithStoreIDGen(opts.IDGen))
	}
	if opts.Clock != nil {
		storeOpts = append(storeOpts, session.WithStoreClock(opts.Clock))
	}
	store, err := session.NewFileStore(storeOpts...)
	if err != nil {
		return nil, fmt.Errorf("runner: open session store: %w", err)
	}
	return store, nil
}

// build assembles a Runner around an already-opened journal: it resolves the
// provider and tool registry, starts the broker and its journaling consumer,
// and (when resumed) publishes session.resumed.
func build(opts Options, store *session.FileStore, journal *session.Journal, resumed bool) (*Runner, error) {
	prov := opts.Provider
	if prov == nil {
		var err error
		prov, err = newProvider(opts.Model, opts.Root)
		if err != nil {
			_ = journal.Close()
			_ = store.Close()
			return nil, err
		}
	}

	tools := opts.Tools
	if tools == nil {
		tools = loop.FromRegistry(tool.NewRegistry(tool.Builtins(opts.Cwd)...))
	}

	broker := event.NewBroker(event.WithReplay(defaultReplay))
	journalSub := broker.Subscribe(event.FilterMustDeliver, defaultSubBuffer)

	r := &Runner{
		model:       opts.Model,
		system:      opts.System,
		params:      opts.Params,
		maxIters:    opts.MaxIters,
		provider:    prov,
		tools:       tools,
		broker:      broker,
		journal:     journal,
		store:       store,
		journalDone: make(chan struct{}),
	}
	go r.consume(journalSub)

	if resumed {
		broker.Publish(event.NewSessionResumed(journal.ID()))
	}
	return r, nil
}

// ID returns the session's journal id.
func (r *Runner) ID() string { return r.journal.ID() }

// JournalPath returns the session journal's JSONL file path.
func (r *Runner) JournalPath() string { return r.journal.Path() }

// Fold returns the session's current folded context — the same projection
// Prompt feeds the provider, exposed for read-only transcript views (e.g.
// `gofer resume <id>` with no prompt).
func (r *Runner) Fold() []session.ContextMessage { return r.journal.Fold() }

// Events returns a subscription to every event the session emits, of both
// delivery tiers.
func (r *Runner) Events() *event.Subscription {
	return r.broker.Subscribe(event.FilterAll, defaultSubBuffer)
}

// Prompt appends text as a user message, projects the journal's folded
// context into provider messages, and drives the agent loop. Loop events
// stream into the journal concurrently as each turn settles (see consume);
// a cancelled ctx interrupts the loop between or during model calls, leaving
// whatever prefix had already settled durably on disk.
func (r *Runner) Prompt(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := r.journal.Append(session.NewMessageEntry("user", text)); err != nil {
		return fmt.Errorf("runner: append user message: %w", err)
	}

	cfg := loop.Config{
		Provider:  r.provider,
		Model:     r.model,
		System:    r.system,
		Params:    r.params,
		Tools:     r.tools,
		Broker:    r.broker,
		SessionID: r.journal.ID(),
		MaxIters:  r.maxIters,
	}
	_, err := loop.Run(ctx, cfg, r.projectMessages())
	return err
}

// Close shuts down the runner's broker (closing every subscription,
// including the journaling consumer's), waits for the journaling consumer to
// drain so no settled turn is lost, then closes the journal and the store.
// It returns the first error encountered, if any, joined with any journal
// write error the consumer observed.
func (r *Runner) Close() error {
	r.broker.Close()
	<-r.journalDone

	var errs []error
	if err := r.journalWriteErr(); err != nil {
		errs = append(errs, err)
	}
	if err := r.journal.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := r.store.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// setJournalWriteErr records the first journal write failure the consumer
// goroutine observes; later failures are dropped (the first is the one that
// matters for diagnosis).
func (r *Runner) setJournalWriteErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jerr == nil {
		r.jerr = err
	}
}

// journalWriteErr returns the first journal write failure the consumer
// goroutine observed, if any.
func (r *Runner) journalWriteErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jerr
}
