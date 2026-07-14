package telemetry_test

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/telemetry"
)

// TestSetup_DisabledByDefault is the "zero otel activity" proof: a zero-value
// Config (Enabled: false) yields a Provider whose tracer produces
// non-recording spans, whose Instrument runs a full scripted stream and
// touches zero real spans, whose Shutdown is a no-op, and which never
// mutates otel's global tracer/meter provider.
func TestSetup_DisabledByDefault(t *testing.T) {
	beforeTracerProvider := otel.GetTracerProvider()
	beforeMeterProvider := otel.GetMeterProvider()

	p, err := telemetry.Setup(context.Background(), telemetry.Config{}, nil)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if otel.GetTracerProvider() != beforeTracerProvider {
		t.Error("Setup(disabled) mutated the global tracer provider")
	}
	if otel.GetMeterProvider() != beforeMeterProvider {
		t.Error("Setup(disabled) mutated the global meter provider")
	}

	_, span := p.Tracer().Start(context.Background(), "whatever")
	if span.IsRecording() {
		t.Error("disabled Provider's tracer produced a recording span")
	}
	span.End()

	// Run a full scripted stream through Instrument: it must not panic and
	// must produce no observable effect on any real recorder — there is none
	// attached, by construction, since the disabled path never builds a real
	// TracerProvider for anything to attach to.
	events := make(chan event.Event, 8)
	events <- event.NewTurnStarted("sess-1")
	events <- event.NewMessageStarted("sess-1", event.MessageText)
	events <- event.NewToolCallStarted("sess-1", "call-A", "bash", json.RawMessage(`{}`))
	events <- event.NewToolCallFinished("sess-1", "call-A", json.RawMessage(`{}`), "ok", false, nil)
	events <- event.NewMessageFinished("sess-1", event.MessageText, "hello")
	events <- event.NewTurnFinished("sess-1", string(provider.StopEndTurn), provider.Usage{InputTokens: 1, OutputTokens: 1})
	close(events)
	p.Instrument(context.Background(), "sess-1", events)

	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on the disabled path returned an error: %v", err)
	}
}

// TestSetup_DisabledUsesNoopTracer double-checks the disabled path's tracer
// is otel's noop tracer specifically (not merely non-recording by
// coincidence): starting a span under a real recorder-backed parent context
// must not produce any span the recorder observes, because the noop tracer
// never calls into a real SpanProcessor at all.
func TestSetup_DisabledUsesNoopTracer(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	realTracer := tp.Tracer("real")

	// A real, recording parent span establishes a valid ctx to start the
	// disabled provider's span under.
	parentCtx, parentSpan := realTracer.Start(context.Background(), "parent")

	p, err := telemetry.Setup(context.Background(), telemetry.Config{}, nil)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, span := p.Tracer().Start(parentCtx, "child")
	span.End()
	parentSpan.End()

	// Only the real parent span should ever have reached the recorder — the
	// disabled tracer's "child" span is a noop that never touches it.
	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorder observed %d ended spans, want exactly 1 (the real parent only): %v", len(ended), ended)
	}
	if ended[0].Name() != "parent" {
		t.Errorf("recorder's one ended span is named %q, want \"parent\"", ended[0].Name())
	}
}
