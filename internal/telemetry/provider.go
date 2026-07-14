package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// instrumentationName identifies gofer's tracer/meter scope.
const instrumentationName = "github.com/jedwards1230/gofer/internal/telemetry"

// Provider bundles the tracer, meter, and derived [Metrics] instrument set
// gofer's telemetry uses, plus a Shutdown that flushes and releases whatever
// [Setup] built.
type Provider struct {
	tracer  trace.Tracer
	Metrics *Metrics

	shutdown func(context.Context) error
}

// Tracer returns the provider's tracer. Never nil: on the disabled path it
// is otel's noop tracer.
func (p *Provider) Tracer() trace.Tracer { return p.tracer }

// Shutdown flushes and releases any exporters/providers Setup built. It is a
// no-op (returns nil) on the disabled path, and safe to call on a nil
// Provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// NewProvider builds a Provider directly over an already-constructed tracer
// and meter, bypassing Setup's exporter/env wiring entirely. This is the
// seam tests use to attach an in-memory span recorder or a manual metric
// reader. Shutdown on a Provider built this way is a no-op — the caller owns
// the tracer/meter providers' lifecycle.
func NewProvider(tracer trace.Tracer, meter metric.Meter) (*Provider, error) {
	metrics, err := NewMetrics(meter)
	if err != nil {
		return nil, err
	}
	return &Provider{tracer: tracer, Metrics: metrics}, nil
}

// Setup builds a Provider from cfg. cfg.Enabled=false (including the zero
// value) returns a Provider backed by otel's noop tracer/meter providers:
// zero exporters, zero background readers/batchers, zero network activity,
// and no global otel state is touched — see the package doc's Configuration
// section. logger receives a couple of startup diagnostics (enabled/
// disabled, endpoint, protocol) — never event content. A nil logger
// discards them.
func Setup(ctx context.Context, cfg Config, logger *slog.Logger) (*Provider, error) {
	cfg = cfg.withEnvDefaults()
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	if !cfg.Enabled {
		logger.Debug("telemetry disabled")
		tracer := tracenoop.NewTracerProvider().Tracer(instrumentationName)
		meter := metricnoop.NewMeterProvider().Meter(instrumentationName)
		return NewProvider(tracer, meter)
	}

	logger.Info("telemetry enabled",
		"endpoint", cfg.Endpoint, "protocol", cfg.Protocol, "service_name", cfg.ServiceName, "insecure", cfg.Insecure)

	res := resource.NewSchemaless(semconv.ServiceName(cfg.ServiceName))

	spanExporter, err := newSpanExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build span exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExporter),
		sdktrace.WithResource(res),
	)

	metricExporter, err := newMetricExporter(ctx, cfg)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: build metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)

	tracer := tp.Tracer(instrumentationName)
	meter := mp.Meter(instrumentationName)
	metrics, err := NewMetrics(meter)
	if err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: build metrics: %w", err)
	}

	return &Provider{
		tracer:  tracer,
		Metrics: metrics,
		shutdown: func(ctx context.Context) error {
			var errs []error
			if err := tp.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
			if err := mp.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		},
	}, nil
}

// New is a small convenience over Setup for daemon wiring: it builds the
// Provider and returns a *slog.Logger wrapping baseHandler with
// [NewLogHandler], so a caller gets both in one call.
func New(ctx context.Context, cfg Config, baseHandler slog.Handler) (*Provider, *slog.Logger, error) {
	p, err := Setup(ctx, cfg, slog.New(baseHandler))
	if err != nil {
		return nil, nil, err
	}
	return p, slog.New(NewLogHandler(baseHandler)), nil
}

// newSpanExporter builds the OTLP trace exporter named by cfg.Protocol.
func newSpanExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	if isHTTPProtocol(cfg.Protocol) {
		opts := []otlptracehttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(endpointURL(cfg)))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		return otlptracehttp.New(ctx, opts...)
	}
	opts := []otlptracegrpc.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracegrpc.WithEndpointURL(endpointURL(cfg)))
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}
	return otlptracegrpc.New(ctx, opts...)
}

// newMetricExporter builds the OTLP metric exporter named by cfg.Protocol.
func newMetricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	if isHTTPProtocol(cfg.Protocol) {
		opts := []otlpmetrichttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(endpointURL(cfg)))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
		}
		return otlpmetrichttp.New(ctx, opts...)
	}
	opts := []otlpmetricgrpc.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlpmetricgrpc.WithEndpointURL(endpointURL(cfg)))
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
	}
	return otlpmetricgrpc.New(ctx, opts...)
}
