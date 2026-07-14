package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jedwards1230/agent-sdk-go/provider"
)

// Metrics bundles gofer's session/turn/token/cost/error instrument set. All
// recorded attributes are counts, enums, or booleans — never content (see
// the package doc's redaction rule).
type Metrics struct {
	sessionsActive metric.Int64UpDownCounter
	turns          metric.Int64Counter
	tokens         metric.Int64Counter
	costUSD        metric.Float64Counter
	errors         metric.Int64Counter
}

// NewMetrics builds gofer's instrument set from meter. Exported as the seam
// tests use to build a [Metrics] directly over an in-memory meter (e.g. one
// backed by an [go.opentelemetry.io/otel/sdk/metric.ManualReader]).
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	sessionsActive, err := meter.Int64UpDownCounter("gofer.sessions.active",
		metric.WithDescription("sessions currently attached to a live event stream"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build gofer.sessions.active: %w", err)
	}
	turns, err := meter.Int64Counter("gofer.turns",
		metric.WithDescription("turns completed, by stop reason"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build gofer.turns: %w", err)
	}
	tokens, err := meter.Int64Counter("gofer.tokens",
		metric.WithDescription("tokens consumed, by type (input/output)"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build gofer.tokens: %w", err)
	}
	costUSD, err := meter.Float64Counter("gofer.cost.usd",
		metric.WithDescription("priced turn cost accrued, in USD"),
		metric.WithUnit("{USD}"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build gofer.cost.usd: %w", err)
	}
	errs, err := meter.Int64Counter("gofer.errors",
		metric.WithDescription("errors observed, by kind"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build gofer.errors: %w", err)
	}
	return &Metrics{
		sessionsActive: sessionsActive,
		turns:          turns,
		tokens:         tokens,
		costUSD:        costUSD,
		errors:         errs,
	}, nil
}

// sessionStarted records one more live-instrumented session.
func (m *Metrics) sessionStarted(ctx context.Context) {
	m.sessionsActive.Add(ctx, 1)
}

// sessionEnded records one fewer live-instrumented session.
func (m *Metrics) sessionEnded(ctx context.Context) {
	m.sessionsActive.Add(ctx, -1)
}

// recordTurn records a completed turn's stop reason, token usage, and (when
// priced) cost.
func (m *Metrics) recordTurn(ctx context.Context, stopReason string, usage provider.Usage, cost *provider.Cost) {
	m.turns.Add(ctx, 1, metric.WithAttributes(attribute.String("stop_reason", stopReason)))
	m.tokens.Add(ctx, int64(usage.InputTokens), metric.WithAttributes(attribute.String("type", "input")))
	m.tokens.Add(ctx, int64(usage.OutputTokens), metric.WithAttributes(attribute.String("type", "output")))
	if cost != nil {
		m.costUSD.Add(ctx, cost.USD)
	}
}

// recordSessionError records a session.error observation (attr fatal only —
// never the error text).
func (m *Metrics) recordSessionError(ctx context.Context, fatal bool) {
	m.errors.Add(ctx, 1, metric.WithAttributes(attribute.Bool("fatal", fatal)))
}

// recordToolError records an erroring tool.call.finished (attr kind=tool).
func (m *Metrics) recordToolError(ctx context.Context) {
	m.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "tool")))
}
