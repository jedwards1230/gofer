package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jedwards1230/gofer/internal/telemetry"
)

// TestNewLogHandler_WithActiveSpan starts a real recording span, logs through
// a JSON handler wrapped with NewLogHandler using the span's context, and
// asserts the record's trace_id/span_id match the span's own ids exactly.
func TestNewLogHandler_WithActiveSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	var buf bytes.Buffer
	logger := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil)))

	ctx, span := tracer.Start(context.Background(), "test-span")
	logger.InfoContext(ctx, "hello")
	span.End()

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v (line: %s)", err, buf.String())
	}

	gotTraceID, _ := rec["trace_id"].(string)
	gotSpanID, _ := rec["span_id"].(string)
	sc := span.SpanContext()
	if gotTraceID != sc.TraceID().String() {
		t.Errorf("trace_id = %q, want %q", gotTraceID, sc.TraceID().String())
	}
	if gotSpanID != sc.SpanID().String() {
		t.Errorf("span_id = %q, want %q", gotSpanID, sc.SpanID().String())
	}
}

// TestNewLogHandler_NoActiveSpan asserts a log call made with a plain
// background context (no span) gets no trace_id/span_id — the handler must
// not fabricate correlation that isn't there.
func TestNewLogHandler_NoActiveSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v (line: %s)", err, buf.String())
	}
	if _, ok := rec["trace_id"]; ok {
		t.Errorf("trace_id present with no active span: %v", rec)
	}
	if _, ok := rec["span_id"]; ok {
		t.Errorf("span_id present with no active span: %v", rec)
	}
}

// TestNewLogHandler_WithAttrsAndGroup asserts WithAttrs/WithGroup delegate
// to the inner handler (attrs/group survive) while still preserving trace
// correlation on the returned handler.
func TestNewLogHandler_WithAttrsAndGroup(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	var buf bytes.Buffer
	base := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	logger := base.With("component", "test").WithGroup("g")

	ctx, span := tracer.Start(context.Background(), "test-span")
	logger.InfoContext(ctx, "hello", "k", "v")
	span.End()

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v (line: %s)", err, buf.String())
	}
	// "component" was attached via With() BEFORE WithGroup("g"), so it stays
	// top-level — proving WithAttrs delegated to the inner handler.
	if rec["component"] != "test" {
		t.Errorf("component attr missing/wrong after WithAttrs delegation: %v", rec)
	}
	// Everything logged at/after the call site — the "k"/"v" attr AND the
	// trace_id/span_id NewLogHandler adds in Handle — falls inside the open
	// "g" group, proving WithGroup delegated to the inner handler too.
	group, ok := rec["g"].(map[string]any)
	if !ok {
		t.Fatalf("no \"g\" group in log line after WithGroup delegation: %v", rec)
	}
	if group["k"] != "v" {
		t.Errorf("grouped attr missing/wrong after WithGroup delegation: %v", group)
	}
	if _, ok := group["trace_id"]; !ok {
		t.Errorf("trace_id missing from group %q after WithAttrs/WithGroup wrapping: %v", "g", group)
	}
}
