package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Protocol selects the OTLP transport used by the exporter.
const (
	// ProtocolGRPC exports spans over OTLP/gRPC (the default).
	ProtocolGRPC = "grpc"
	// ProtocolHTTP exports spans over OTLP/HTTP (protobuf).
	ProtocolHTTP = "http"
)

// defaultServiceName is the resource service.name used when OTLPConfig leaves
// ServiceName empty.
const defaultServiceName = "ozzharness"

// OTLPConfig configures the OTLP trace exporter and the SDK TracerProvider that
// Setup installs. A zero Endpoint disables tracing entirely.
type OTLPConfig struct {
	// Endpoint is the collector address, e.g. "localhost:4317" for gRPC or a
	// URL/host for HTTP. An empty Endpoint disables tracing: Setup installs
	// nothing and returns a no-op shutdown.
	Endpoint string
	// Protocol selects the transport: "grpc" (default) or "http". Any other
	// value is rejected by Setup.
	Protocol string
	// Insecure skips TLS when dialing the collector (development only).
	Insecure bool
	// ServiceName sets the resource service.name attribute. Defaults to
	// "ozzharness" when empty.
	ServiceName string
	// Headers are sent with every export request (e.g. auth headers).
	Headers map[string]string
	// Timeout bounds a single export request. Zero uses the exporter default.
	Timeout time.Duration
	// SampleRatio is the parent-based head-sampling ratio in [0,1]. Values <= 0
	// select always-on sampling (the default); values >= 1 also sample every
	// trace. Non-root spans follow their parent's sampling decision.
	SampleRatio float64
	// Version sets the resource service.version attribute when non-empty.
	Version string
}

// Setup builds an OTLP span exporter and an SDK TracerProvider with a batch span
// processor, a parent-based sampler, and a resource carrying service.name and
// (optionally) service.version. It installs the provider via otel.SetTracerProvider
// and a W3C TraceContext propagator via otel.SetTextMapPropagator, then returns a
// shutdown func that flushes and stops the provider.
//
// When cfg.Endpoint is empty, tracing is disabled: Setup installs nothing and
// returns a no-op shutdown with a nil error (the caller should log that tracing
// is disabled). After Setup runs with a real endpoint, telemetry.NewTracing
// reading otel.GetTracerProvider() emits to the installed provider.
//
// The gRPC exporter is constructed lazily and does not dial the collector until
// the first export, so Setup returns promptly even against an unreachable
// endpoint.
func Setup(ctx context.Context, cfg OTLPConfig) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	if cfg.Endpoint == "" {
		return noop, nil
	}

	exporter, err := newExporter(ctx, cfg)
	if err != nil {
		return noop, fmt.Errorf("telemetry: build OTLP exporter: %w", err)
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		// Best effort: tear down the exporter we just created.
		_ = exporter.Shutdown(ctx)
		return noop, fmt.Errorf("telemetry: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler(cfg.SampleRatio)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// newExporter constructs the OTLP span exporter for the configured protocol.
func newExporter(ctx context.Context, cfg OTLPConfig) (*otlptrace.Exporter, error) {
	switch cfg.Protocol {
	case "", ProtocolGRPC:
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		if cfg.Timeout > 0 {
			opts = append(opts, otlptracegrpc.WithTimeout(cfg.Timeout))
		}
		return otlptracegrpc.New(ctx, opts...)
	case ProtocolHTTP:
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		if cfg.Timeout > 0 {
			opts = append(opts, otlptracehttp.WithTimeout(cfg.Timeout))
		}
		return otlptracehttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown OTLP protocol %q (want %q or %q)",
			cfg.Protocol, ProtocolGRPC, ProtocolHTTP)
	}
}

// newResource builds the OTel resource describing this service.
func newResource(ctx context.Context, cfg OTLPConfig) (*resource.Resource, error) {
	name := cfg.ServiceName
	if name == "" {
		name = defaultServiceName
	}
	attrs := []attribute.KeyValue{semconv.ServiceName(name)}
	if cfg.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.Version))
	}
	return resource.New(ctx,
		resource.WithAttributes(attrs...),
	)
}

// sampler returns a parent-based sampler. A ratio <= 0 selects always-on; a
// ratio >= 1 also samples every trace; values in between select a trace-ID-ratio
// root sampler.
func sampler(ratio float64) sdktrace.Sampler {
	switch {
	case ratio <= 0, ratio >= 1:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}
