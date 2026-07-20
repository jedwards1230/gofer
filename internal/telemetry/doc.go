// Package telemetry is gofer's OpenTelemetry integration: traces, metrics,
// and slog trace-correlation built entirely off the SDK's typed Event/Op
// stream. The SDK ([github.com/jedwards1230/agent-sdk-go]) stays
// dependency-light and knows nothing about tracing — this package is the
// only place in gofer that imports go.opentelemetry.io/*.
//
// # Span model
//
// One session's event stream is consumed by [Provider.Instrument], which
// runs a small state machine tracking a single "current open turn span" per
// session:
//
//   - turn.started opens a root span named "turn" (attr session.id).
//   - message.started/message.finished (kind != user) opens/closes a child
//     span named "model" (attr message.kind) — this is the closest available
//     proxy for a provider call; the event contract has no dedicated
//     provider-call event (see Gaps below).
//   - tool.call.started/tool.call.finished opens/closes a child span named
//     "tool" (attrs tool.name, tool.call.id), keyed by call id so concurrent
//     tool calls within a turn are tracked independently. An erroring call
//     sets the span's status to Error and a tool.error=true attribute — never
//     the tool's result text.
//   - turn.finished records the turns/tokens/cost metrics (errors are not
//     among them — they come from session.error and erroring tool calls),
//     stamps
//     stop_reason on the turn span, and ends it — defensively ending any
//     still-open model/tool child spans first.
//   - session.error adds a session.error span event (attr fatal only) to the
//     open turn span, if any, and records the errors metric.
//   - message.delta and tool.call.delta are ignored (lossy, no span value).
//   - On channel close or context cancellation, every still-open span is
//     ended so nothing leaks.
//
// Instrument is session-scoped and relies on the supervisor's serial
// per-session pump: exactly one turn is ever in flight per session at a
// time, so tracking "the current open turn" is sufficient — no turn id is
// needed (see Gaps below).
//
// # The hard redaction rule
//
// Span attributes, span names, span events, and log attributes carry ONLY
// ids, names, kinds, counts, durations, costs, stop-reasons, and booleans —
// never prompt text, tool input/output, message content, permission specs,
// or token/bearer contents. Concretely, the instrumenter reads only .ID,
// .Name, .MessageKind, .StopReason, .Usage, .Cost, .IsError, .Fatal, and the
// envelope accessors (.Kind(), .SessionID()); it never reads .Input,
// .Result, .Content, .Text, .Delta, .Diagnostics, .Spec, .Trace, .Meta, or
// .SpillPath. This is a hard invariant, not a style preference — see
// instrument_test.go's redaction test.
//
// # Gaps (flagged, not invented)
//
// Two limitations fall directly out of the current Event/Op contract and are
// deliberately not worked around by fabricating data:
//
//  1. Turn and tool events carry no turn id. Correlation across a turn's
//     spans relies entirely on the supervisor's serial per-session pump (one
//     turn in flight at a time) rather than an explicit identifier. A
//     terminal turn.finished can also arrive with no preceding turn.started
//     (the loop's max-turns cap) — Instrument records turn/token/cost
//     metrics in that case but opens and ends no span.
//  2. There is no dedicated provider-call event. The message.* pair is the
//     closest proxy, and per-provider-call token usage is not available —
//     provider.Usage is a turn-aggregate carried only on turn.finished. The
//     "model" span therefore has no cost/token attributes of its own.
//
// # Configuration
//
// [Config] is off by default (Enabled: false). [Config.withEnvDefaults]
// layers in the standard OTel environment variables
// (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_PROTOCOL,
// OTEL_SERVICE_NAME, OTEL_EXPORTER_OTLP_HEADERS) for any field left unset —
// but Enabled is gofer's own gate and is never turned on by an environment
// variable; a deployment must opt in explicitly. [Setup] with a disabled
// config returns a [Provider] backed entirely by otel's noop tracer/meter
// providers: no exporter is built, no background reader or batcher starts,
// no global otel state is touched, and no network connection is ever
// attempted.
//
// # Log correlation
//
// [NewLogHandler] wraps an [log/slog.Handler] so a log record made with a
// context carrying an active, sampled span gets trace_id/span_id attributes
// stamped in. It only sees spans on the ctx a given log call actually
// carries — the instrumenter's turn/model/tool spans live in their own
// per-session goroutine context, so broad correlation across every daemon
// log site is future work; M3 ships the handler and proves it works.
package telemetry
