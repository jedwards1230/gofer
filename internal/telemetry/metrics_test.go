package telemetry_test

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// TestMetrics_TwoTurnsToolErrorSessionError drives two full turns (with
// known usage/cost), one erroring tool call, and one session.error through
// Instrument, then asserts every instrument's collected value.
func TestMetrics_TwoTurnsToolErrorSessionError(t *testing.T) {
	p, _, reader := newTestProvider(t)

	runInstrument(t, p, "sess-1", func(events chan<- event.Event) {
		// Turn 1: a tool call that errors.
		events <- event.NewTurnStarted("sess-1")
		events <- event.NewToolCallStarted("sess-1", "call-A", "bash", json.RawMessage(`{}`))
		events <- event.NewToolCallFinished("sess-1", "call-A", json.RawMessage(`{}`), "boom", true, nil)
		events <- event.NewSessionError("sess-1", "transient failure", false)
		events <- event.NewTurnFinishedCost("sess-1", string(provider.StopEndTurn),
			provider.Usage{InputTokens: 10, OutputTokens: 20}, &provider.Cost{USD: 0.05})

		// Turn 2: clean.
		events <- event.NewTurnStarted("sess-1")
		events <- event.NewTurnFinishedCost("sess-1", string(provider.StopEndTurn),
			provider.Usage{InputTokens: 3, OutputTokens: 4}, &provider.Cost{USD: 0.01})
	})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if got := sumInt64(t, rm, "gofer.turns"); got != 2 {
		t.Errorf("gofer.turns = %d, want 2", got)
	}

	inputTokens := sumInt64Attr(t, rm, "gofer.tokens", "type", "input")
	if inputTokens != 13 {
		t.Errorf("gofer.tokens{type=input} = %d, want 13 (10 + 3)", inputTokens)
	}
	outputTokens := sumInt64Attr(t, rm, "gofer.tokens", "type", "output")
	if outputTokens != 24 {
		t.Errorf("gofer.tokens{type=output} = %d, want 24 (20 + 4)", outputTokens)
	}

	costSum := sumFloat64(t, rm, "gofer.cost.usd")
	const wantCost = 0.06
	if diff := costSum - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("gofer.cost.usd = %v, want %v", costSum, wantCost)
	}

	// One tool error (kind=tool) + one session error (fatal=false) == 2.
	if got := sumInt64(t, rm, "gofer.errors"); got != 2 {
		t.Errorf("gofer.errors = %d, want 2", got)
	}
}

// TestMetrics_SessionsActiveNetZero asserts gofer.sessions.active nets back
// to zero once the event stream closes, having gone to 1 while open.
func TestMetrics_SessionsActiveNetZero(t *testing.T) {
	p, _, reader := newTestProvider(t)

	events := make(chan event.Event)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Instrument(context.Background(), "sess-1", events)
	}()

	events <- event.NewTurnStarted("sess-1")
	events <- event.NewTurnFinished("sess-1", string(provider.StopEndTurn), provider.Usage{})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sumInt64(t, rm, "gofer.sessions.active"); got != 1 {
		t.Errorf("gofer.sessions.active while open = %d, want 1", got)
	}

	close(events)
	<-done

	rm = metricdata.ResourceMetrics{}
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sumInt64(t, rm, "gofer.sessions.active"); got != 0 {
		t.Errorf("gofer.sessions.active after close = %d, want 0 (net zero)", got)
	}
}

// sumInt64Attr sums only the data points matching attrKey=attrVal for the
// named int64 counter, so a multi-attribute instrument like gofer.tokens
// (type=input vs type=output) can be checked per attribute value.
func sumInt64Attr(t *testing.T, rm metricdata.ResourceMetrics, name, attrKey, attrVal string) int64 {
	t.Helper()
	m, ok := findMetric(rm, name)
	if !ok {
		t.Fatalf("metric %q not found", name)
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q is not an int64 Sum aggregation (got %T)", name, m.Data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		if v, ok := dp.Attributes.Value(attribute.Key(attrKey)); ok && v.AsString() == attrVal {
			total += dp.Value
		}
	}
	return total
}

// sumFloat64 sums every data point across every attribute set for the named
// float64 counter instrument in rm.
func sumFloat64(t *testing.T, rm metricdata.ResourceMetrics, name string) float64 {
	t.Helper()
	m, ok := findMetric(rm, name)
	if !ok {
		t.Fatalf("metric %q not found", name)
	}
	sum, ok := m.Data.(metricdata.Sum[float64])
	if !ok {
		t.Fatalf("metric %q is not a float64 Sum aggregation (got %T)", name, m.Data)
	}
	var total float64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}
