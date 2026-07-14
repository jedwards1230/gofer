package telemetry_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/telemetry"
)

// newTestProvider builds a *telemetry.Provider wired directly over an
// in-memory span recorder and a manual metric reader — the seam
// [telemetry.NewProvider] exists for. Returns the recorder and reader
// alongside so tests can inspect ended spans and collect metrics.
func newTestProvider(t *testing.T) (*telemetry.Provider, *tracetest.SpanRecorder, *metric.ManualReader) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	p, err := telemetry.NewProvider(tp.Tracer("test"), mp.Meter("test"))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p, recorder, reader
}

// runInstrument feeds events through p.Instrument for sessionID, closing
// events when the sender func returns, and blocks until Instrument itself
// returns.
func runInstrument(t *testing.T, p *telemetry.Provider, sessionID string, send func(chan<- event.Event)) {
	t.Helper()
	events := make(chan event.Event)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Instrument(context.Background(), sessionID, events)
	}()
	send(events)
	close(events)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Instrument did not return after channel close")
	}
}

// spansByName indexes recorder's ended spans by name for lookup; fails the
// test if a name appears more than once (tests that expect exactly one span
// per name use this to assert that directly).
func spansByName(t *testing.T, recorder *tracetest.SpanRecorder) map[string]sdktrace.ReadOnlySpan {
	t.Helper()
	out := make(map[string]sdktrace.ReadOnlySpan)
	for _, s := range recorder.Ended() {
		if _, dup := out[s.Name()]; dup {
			t.Fatalf("more than one ended span named %q", s.Name())
		}
		out[s.Name()] = s
	}
	return out
}

// sumInt64 sums every data point across every attribute set for the named
// int64 counter/up-down-counter instrument in rm. Fails the test if the
// instrument is not found or is not an int64 Sum aggregation.
func sumInt64(t *testing.T, rm metricdata.ResourceMetrics, name string) int64 {
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
		total += dp.Value
	}
	return total
}

// findMetric locates the named instrument's aggregated data within rm.
func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func TestInstrument_SpanOpenClosePairing(t *testing.T) {
	p, recorder, _ := newTestProvider(t)

	runInstrument(t, p, "sess-1", func(events chan<- event.Event) {
		events <- event.NewTurnStarted("sess-1")
		events <- event.NewMessageStarted("sess-1", event.MessageText)
		events <- event.NewToolCallStarted("sess-1", "call-A", "bash", json.RawMessage(`{}`))
		events <- event.NewToolCallFinished("sess-1", "call-A", json.RawMessage(`{}`), "ok", false, nil)
		events <- event.NewMessageFinished("sess-1", event.MessageText, "hello")
		events <- event.NewTurnFinished("sess-1", string(provider.StopEndTurn), provider.Usage{InputTokens: 1, OutputTokens: 2})
	})

	spans := spansByName(t, recorder)
	turn, ok := spans["turn"]
	if !ok {
		t.Fatal("no ended \"turn\" span")
	}
	model, ok := spans["model"]
	if !ok {
		t.Fatal("no ended \"model\" span")
	}
	tool, ok := spans["tool"]
	if !ok {
		t.Fatal("no ended \"tool\" span")
	}
	if len(spans) != 3 {
		t.Fatalf("want exactly 3 ended spans (turn, model, tool), got %d: %v", len(spans), spans)
	}

	for name, s := range spans {
		if s.EndTime().IsZero() {
			t.Errorf("span %q not ended", name)
		}
	}

	for name, child := range map[string]sdktrace.ReadOnlySpan{"model": model, "tool": tool} {
		if child.Parent().TraceID() != turn.SpanContext().TraceID() {
			t.Errorf("%s span: TraceID %s != turn's %s", name, child.Parent().TraceID(), turn.SpanContext().TraceID())
		}
		if child.Parent().SpanID() != turn.SpanContext().SpanID() {
			t.Errorf("%s span: ParentSpanID %s != turn's SpanID %s", name, child.Parent().SpanID(), turn.SpanContext().SpanID())
		}
	}
}

func TestInstrument_UnmatchedTurnFinished(t *testing.T) {
	p, recorder, reader := newTestProvider(t)

	// The loop's max-turns cap emits a terminal turn.finished with no
	// preceding turn.started — must not panic, must not open/leak a span,
	// and must still record metrics.
	runInstrument(t, p, "sess-1", func(events chan<- event.Event) {
		events <- event.NewTurnFinished("sess-1", string(provider.StopMaxTurns), provider.Usage{InputTokens: 5, OutputTokens: 7})
	})

	if got := len(recorder.Started()); got != 0 {
		t.Errorf("want 0 started spans for an unmatched turn.finished, got %d", got)
	}
	if got := len(recorder.Ended()); got != 0 {
		t.Errorf("want 0 ended spans for an unmatched turn.finished, got %d", got)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sumInt64(t, rm, "gofer.turns"); got != 1 {
		t.Errorf("gofer.turns = %d, want 1 (metrics must still record on an unmatched turn.finished)", got)
	}
	if got := sumInt64(t, rm, "gofer.tokens"); got != 12 {
		t.Errorf("gofer.tokens = %d, want 12 (5 input + 7 output)", got)
	}
}

func TestInstrument_DanglingSpanCleanup(t *testing.T) {
	p, recorder, _ := newTestProvider(t)

	// turn.started + tool.call.started with no matching finished events, then
	// the channel closes without a turn.finished at all: both spans must
	// still be ended, not leaked.
	runInstrument(t, p, "sess-1", func(events chan<- event.Event) {
		events <- event.NewTurnStarted("sess-1")
		events <- event.NewToolCallStarted("sess-1", "call-A", "bash", json.RawMessage(`{}`))
	})

	spans := spansByName(t, recorder)
	for _, name := range []string{"turn", "tool"} {
		s, ok := spans[name]
		if !ok {
			t.Fatalf("no ended %q span after channel close", name)
		}
		if s.EndTime().IsZero() {
			t.Errorf("span %q not ended", name)
		}
	}
}

// TestInstrument_Redaction is the hard-redaction-rule proof: every field the
// event contract carries that might hold sensitive content is scripted with
// a scanning marker, and every recorded span's name, attributes, and events
// are scanned afterward to confirm the marker never leaked through.
func TestInstrument_Redaction(t *testing.T) {
	const marker = "SEKRET"
	p, recorder, _ := newTestProvider(t)

	runInstrument(t, p, "sess-1", func(events chan<- event.Event) {
		events <- event.NewTurnStarted("sess-1")
		events <- event.NewMessageStarted("sess-1", event.MessageText)
		events <- event.NewToolCallStarted("sess-1", "call-A", "bash", json.RawMessage(`{"cmd":"`+marker+`-IN"}`))
		events <- event.NewPermissionRequested("sess-1", "call-A", "bash",
			map[string]any{"command": marker + "-SPEC"}, []string{marker + "-TRACE"})
		events <- event.NewPermissionResolved("sess-1", "call-A", event.VerdictAllow, marker+"-RULE")
		events <- event.NewToolCallFinished("sess-1", "call-A", json.RawMessage(`{"cmd":"`+marker+`-FIN"}`), marker+"-RESULT", true, []string{marker + "-DIAG"})
		events <- event.NewMessageFinished("sess-1", event.MessageText, marker+"-CONTENT")
		events <- event.NewSessionError("sess-1", marker+"-ERR", false)
		events <- event.NewTurnFinished("sess-1", string(provider.StopEndTurn), provider.Usage{InputTokens: 1, OutputTokens: 1})
	})

	for _, s := range recorder.Ended() {
		if strings.Contains(s.Name(), marker) {
			t.Errorf("span name %q contains the redaction marker", s.Name())
		}
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), marker) {
				t.Errorf("span %q attribute %s=%q contains the redaction marker", s.Name(), kv.Key, kv.Value.String())
			}
		}
		for _, ev := range s.Events() {
			if strings.Contains(ev.Name, marker) {
				t.Errorf("span %q event name %q contains the redaction marker", s.Name(), ev.Name)
			}
			for _, kv := range ev.Attributes {
				if strings.Contains(kv.Value.String(), marker) {
					t.Errorf("span %q event %q attribute %s=%q contains the redaction marker", s.Name(), ev.Name, kv.Key, kv.Value.String())
				}
			}
		}
	}
}
