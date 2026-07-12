// Package runner is gofer's session glue: it builds a real provider and tool
// registry, drives the SDK's agent loop, and streams the loop's typed events
// into a durable session journal as each model-call turn settles. The SDK
// drives the loop and emits events; it does not persist anything. That
// persistence — and folding a journal back into provider messages on resume —
// is gofer-owned, consuming the SDK only through its exported
// provider/auth/tool/loop/event/session APIs (never SDK internals).
//
// Because journaling is event-sourced, stored content blocks carry no per-block
// Meta (e.g. Anthropic reasoning signatures), so reasoning signatures are not
// preserved across a resume boundary — a documented M1 limitation with an M2
// path in docs/M1-PROOF.md.
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

	// Store, when set, is used instead of building a store from Root. This is
	// the seam a multi-session owner (the supervisor) uses to share one
	// *session.FileStore across every Runner it drives: the Runner does NOT
	// close an injected Store in Close — the caller owns its lifecycle. Tests
	// use it too, to share a store across a Runner and out-of-band assertions.
	Store *session.FileStore
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
	// ownsStore is true when this Runner built its own store (Options.Store
	// was nil) and so must close it in Close; false when the store was
	// injected and its lifecycle belongs to the caller.
	ownsStore bool

	journalDone chan struct{}

	mu   sync.Mutex
	jerr error
}

// NewSession builds a Runner around a freshly created journal for the
// project at opts.Cwd. The provider (and its credential) is resolved BEFORE the
// journal is created, so a missing-credential misconfiguration fails fast with
// no orphan journal on disk.
func NewSession(ctx context.Context, opts Options) (*Runner, error) {
	prov, err := resolveProvider(ctx, opts)
	if err != nil {
		return nil, err
	}
	store, err := newStore(opts)
	if err != nil {
		return nil, err
	}
	journal, err := store.Create(ctx, session.Slugify(opts.Cwd))
	if err != nil {
		if opts.Store == nil {
			_ = store.Close()
		}
		return nil, fmt.Errorf("runner: create session: %w", err)
	}
	return build(opts, store, journal, prov, false), nil
}

// Resume builds a Runner around the existing journal for id, publishing
// session.resumed once the runner is live. The provider is resolved before the
// journal is opened so a credential misconfiguration fails before session.resumed.
func Resume(ctx context.Context, id string, opts Options) (*Runner, error) {
	prov, err := resolveProvider(ctx, opts)
	if err != nil {
		return nil, err
	}
	store, err := newStore(opts)
	if err != nil {
		return nil, err
	}
	journal, err := store.Open(ctx, id)
	if err != nil {
		if opts.Store == nil {
			_ = store.Close()
		}
		return nil, fmt.Errorf("runner: open session %s: %w", id, err)
	}
	return build(opts, store, journal, prov, true), nil
}

// resolveProvider returns the test-injected provider when set, else builds the
// real one — which pre-flights its credential. It runs before any journal is
// created so a failure leaves no on-disk residue.
func resolveProvider(ctx context.Context, opts Options) (provider.Provider, error) {
	if opts.Provider != nil {
		return opts.Provider, nil
	}
	return newProvider(ctx, opts.Model, opts.Root)
}

// newStore returns the injected store when opts.Store is set (the caller
// owns its lifecycle — see Options.Store), else builds one from opts, wiring
// the deterministic id generator / clock test seams when set.
func newStore(opts Options) (*session.FileStore, error) {
	if opts.Store != nil {
		return opts.Store, nil
	}
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

// build assembles a Runner around an already-opened journal and a resolved
// provider: it wires the tool registry, starts the broker and its journaling
// consumer, and (when resumed) publishes session.resumed.
func build(opts Options, store *session.FileStore, journal *session.Journal, prov provider.Provider, resumed bool) *Runner {
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
		ownsStore:   opts.Store == nil,
		journalDone: make(chan struct{}),
	}
	go r.consume(journalSub)

	if resumed {
		broker.Publish(event.NewSessionResumed(journal.ID()))
	}
	return r
}

// ID returns the session's journal id.
func (r *Runner) ID() string { return r.journal.ID() }

// JournalPath returns the session journal's JSONL file path.
func (r *Runner) JournalPath() string { return r.journal.Path() }

// Fold returns the session's current folded context as provider messages —
// the same context Prompt feeds the provider, exposed for read-only transcript
// views (e.g. `gofer resume <id>` with no prompt).
func (r *Runner) Fold() []provider.Message { return r.journal.Fold() }

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
	if _, err := r.journal.Append(session.NewMessageEntry(provider.UserText(text))); err != nil {
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
	// The journal folds back to provider messages directly (verbatim content
	// blocks), so the loop's input is the folded context as-is.
	_, err := loop.Run(ctx, cfg, r.journal.Fold())
	return err
}

// Close shuts down the runner's broker (closing every subscription,
// including the journaling consumer's), waits for the journaling consumer to
// drain so no settled turn is lost, then closes the journal and — only when
// this Runner built its own store (Options.Store was nil) — the store. An
// injected store is never closed here; its lifecycle belongs to the caller
// (e.g. the supervisor, which shares one store across many Runners). Close
// returns the first error encountered, if any, joined with any journal write
// error the consumer observed.
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
	if r.ownsStore {
		if err := r.store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Emit publishes a lifecycle event (e.g. session.killed / session.archived)
// onto this session's stream so subscribers observe it. The supervisor calls
// this when it kills or archives a session; it must be called before Close,
// which closes the broker and so ends delivery to every subscriber.
func (r *Runner) Emit(e event.Event) { r.broker.Publish(e) }

// Cost returns the session's token/cost tally across every journaled turn,
// priced against the embedded provider model registry. It is the read model
// the supervisor surfaces per roster row; an unknown (or faux) model still
// has its tokens summed, with a zero priced cost.
func (r *Runner) Cost() session.CostReport { return r.journal.Cost(session.RegistryPricing{}) }

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
