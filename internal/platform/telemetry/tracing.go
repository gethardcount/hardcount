// Package telemetry sets up OpenTelemetry tracing for Hardcount. It also
// holds ContextHandler, which adds the request and tenant IDs to log
// records.
//
// Configure tracing through the environment:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318  (Jaeger, Grafana Tempo, etc.)
//	OTEL_SERVICE_NAME=hardcount                            (defaults to "hardcount")
//	OTEL_TRACES_SAMPLER=parentbased_traceidratio       (defaults to parentbased_always_on)
//	OTEL_TRACES_SAMPLER_ARG=0.1                        (the ratio, for the *traceidratio samplers)
//
// When OTEL_EXPORTER_OTLP_ENDPOINT is not set, tracing is disabled (noop).
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
)

// Init sets up the global OpenTelemetry tracer provider. It returns a
// shutdown function that must be called on application exit. If
// OTEL_EXPORTER_OTLP_ENDPOINT is not set, it returns a no-op, with zero
// overhead.
func Init(ctx context.Context) (shutdown func(context.Context) error, err error) {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		slog.Info("tracing disabled (OTEL_EXPORTER_OTLP_ENDPOINT not set)")
		return func(context.Context) error { return nil }, nil
	}

	serviceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = "hardcount"
	}

	// The service resource is schemaless on purpose. resource.Merge refuses
	// to merge two resources with different schema URLs, and
	// resource.Default() reports whatever schema the SDK ships with. A
	// pinned semconv/vX.Y.Z resource would fail to merge as soon as the SDK's
	// schema moved ahead of the import. Init would return "conflicting Schema
	// URL", which is fatal at boot. Then setting OTEL_EXPORTER_OTLP_ENDPOINT,
	// the documented way to turn tracing on, would stop the server from
	// starting.
	//
	// A schemaless resource merges with anything, so the attribute lands and
	// Default()'s schema is kept. An SDK upgrade cannot break this. Pinning
	// semconv to match the SDK would only hold until the next upgrade.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, err
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}

	// No WithSampler on purpose: the SDK reads OTEL_TRACES_SAMPLER and
	// OTEL_TRACES_SAMPLER_ARG, and an explicit sampler would replace them.
	// Unset, the SDK default is parentbased_always_on (sample everything).
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("tracing enabled", "endpoint", endpoint, "service", serviceName)
	return tp.Shutdown, nil
}

// Tracer returns a named tracer for the given package or component. It is
// safe to call when tracing is disabled, and then returns a no-op tracer.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}
