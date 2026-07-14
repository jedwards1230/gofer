package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// Span names. Kept as untyped constants (not attributes) — a span name
// itself is never content, only a fixed label from this closed set.
const (
	spanTurn  = "turn"
	spanModel = "model"
	spanTool  = "tool"
)

// Instrument consumes one session's event stream, opening/closing spans and
// recording metrics until events is closed or ctx is cancelled. It is
// blocking — run it in its own goroutine, one per session (see the daemon's
// supervisor.Config.OnRegister wiring).
//
// Instrument is session-scoped: it relies on the supervisor's serial
// per-session pump (exactly one turn in flight at a time — see
// internal/supervisor/managed.go's pump), so it needs to track only one
// "current open turn span" rather than correlate by a turn id the event
// contract does not carry (see the package doc's Gaps section).
//
// Every attribute this function reads or emits is an id, name, kind, count,
// duration, cost, stop-reason, or boolean — never prompt/tool/message
// content (see the package doc's redaction rule; instrument_test.go proves
// it).
func (p *Provider) Instrument(ctx context.Context, sessionID string, events <-chan event.Event) {
	st := &sessionInstrumenter{
		ctx:       ctx,
		sessionID: sessionID,
		tracer:    p.tracer,
		metrics:   p.Metrics,
		tools:     make(map[string]trace.Span),
	}
	st.metrics.sessionStarted(ctx)
	defer st.metrics.sessionEnded(ctx)
	defer st.closeAll()

	for {
		select {
		case e, ok := <-events:
			if !ok {
				return
			}
			st.handle(e)
		case <-ctx.Done():
			return
		}
	}
}

// sessionInstrumenter is Instrument's per-session state machine: at most one
// open turn span, at most one open model (provider-call proxy) span nested
// under it, and a set of open tool spans keyed by call id.
type sessionInstrumenter struct {
	ctx       context.Context
	sessionID string
	tracer    trace.Tracer
	metrics   *Metrics

	turnSpan trace.Span
	turnCtx  context.Context

	modelSpan trace.Span

	tools map[string]trace.Span
}

// parentCtx returns the current turn's context to parent a new child span
// under, or the instrumenter's base ctx when no turn is open.
func (st *sessionInstrumenter) parentCtx() context.Context {
	if st.turnSpan != nil {
		return st.turnCtx
	}
	return st.ctx
}

// handle dispatches one event into the state machine. Kinds with no span
// value (message.delta, tool.call.delta, permission.*, session lifecycle
// other than session.error) fall through to the default case and are
// ignored.
func (st *sessionInstrumenter) handle(e event.Event) {
	switch ev := e.(type) {
	case event.TurnStarted:
		st.startTurn()
	case event.MessageStarted:
		if ev.MessageKind == event.MessageUser {
			return
		}
		st.startModel(ev.MessageKind)
	case event.MessageFinished:
		if ev.MessageKind == event.MessageUser {
			return
		}
		st.endModel()
	case event.ToolCallStarted:
		st.startTool(ev.ID, ev.Name)
	case event.ToolCallFinished:
		st.endTool(ev.ID, ev.IsError)
	case event.TurnFinished:
		st.finishTurn(ev.StopReason, ev.Usage, ev.Cost)
	case event.SessionError:
		st.metrics.recordSessionError(st.ctx, ev.Fatal)
		st.annotateSessionError(ev.Fatal)
	}
}

// startTurn opens the root "turn" span. If one is somehow already open
// (shouldn't happen — turn.started events are not expected to nest) it is
// defensively ended first, so a stray double-start never leaks a span.
func (st *sessionInstrumenter) startTurn() {
	if st.turnSpan != nil {
		st.turnSpan.End()
	}
	ctx, span := st.tracer.Start(st.ctx, spanTurn, trace.WithAttributes(attribute.String("session.id", st.sessionID)))
	st.turnCtx = ctx
	st.turnSpan = span
}

// startModel opens the "model" child span — the provider-call proxy (see the
// package doc's Gaps section: the event contract has no dedicated
// provider-call event).
func (st *sessionInstrumenter) startModel(kind event.MessageKind) {
	if st.modelSpan != nil {
		st.modelSpan.End()
	}
	_, span := st.tracer.Start(st.parentCtx(), spanModel, trace.WithAttributes(attribute.String("message.kind", string(kind))))
	st.modelSpan = span
}

// endModel closes the open "model" span, if any.
func (st *sessionInstrumenter) endModel() {
	if st.modelSpan == nil {
		return
	}
	st.modelSpan.End()
	st.modelSpan = nil
}

// startTool opens a "tool" child span for call id, keyed for later lookup by
// endTool.
func (st *sessionInstrumenter) startTool(id, name string) {
	_, span := st.tracer.Start(st.parentCtx(), spanTool, trace.WithAttributes(
		attribute.String("tool.name", name),
		attribute.String("tool.call.id", id),
	))
	st.tools[id] = span
}

// endTool closes the "tool" span matching id, if one is open. An erroring
// call sets the span status to Error (no message text) and a tool.error=true
// attribute, and records the tool-error metric.
func (st *sessionInstrumenter) endTool(id string, isError bool) {
	span, ok := st.tools[id]
	if !ok {
		return
	}
	delete(st.tools, id)
	if isError {
		span.SetStatus(codes.Error, "")
		span.SetAttributes(attribute.Bool("tool.error", true))
		st.metrics.recordToolError(st.ctx)
	}
	span.End()
}

// finishTurn records the turn's metrics, defensively ends any still-open
// model/tool child spans, and ends the turn span — unless none is open (the
// loop's max-turns terminal can emit turn.finished with no matching
// turn.started; see the package doc's Gaps section), in which case only the
// metrics are recorded and nothing is ended.
func (st *sessionInstrumenter) finishTurn(stopReason string, usage provider.Usage, cost *provider.Cost) {
	st.metrics.recordTurn(st.ctx, stopReason, usage, cost)

	// Defensively end any still-open child spans first, regardless of
	// whether a turn span is open.
	if st.modelSpan != nil {
		st.modelSpan.End()
		st.modelSpan = nil
	}
	for id, span := range st.tools {
		span.End()
		delete(st.tools, id)
	}

	if st.turnSpan == nil {
		return
	}
	st.turnSpan.SetAttributes(attribute.String("stop_reason", stopReason))
	st.turnSpan.End()
	st.turnSpan = nil
	st.turnCtx = nil
}

// annotateSessionError adds a session.error span event (attr fatal only —
// never the error text) to the open turn span, if any.
func (st *sessionInstrumenter) annotateSessionError(fatal bool) {
	if st.turnSpan == nil {
		return
	}
	st.turnSpan.AddEvent("session.error", trace.WithAttributes(attribute.Bool("fatal", fatal)))
}

// closeAll ends every still-open span — turn, model, and every open tool —
// so nothing leaks when the event channel closes or ctx is cancelled.
func (st *sessionInstrumenter) closeAll() {
	if st.modelSpan != nil {
		st.modelSpan.End()
		st.modelSpan = nil
	}
	for id, span := range st.tools {
		span.End()
		delete(st.tools, id)
	}
	if st.turnSpan != nil {
		st.turnSpan.End()
		st.turnSpan = nil
	}
}
