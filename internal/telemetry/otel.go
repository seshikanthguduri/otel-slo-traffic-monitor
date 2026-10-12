// Package telemetry wires the OpenTelemetry SDK (traces + metrics) for every
// binary in this repository.
//
// Everything is configured through the standard OTEL_* environment variables
// defined by the OpenTelemetry specification (OTEL_EXPORTER_OTLP_ENDPOINT,
// OTEL_RESOURCE_ATTRIBUTES, OTEL_TRACES_SAMPLER, OTEL_METRIC_EXPORT_INTERVAL,
// ...). Nothing here is cloud specific: the same binary runs on minikube, EKS,
// GKE, AKS or bare metal, and the Collector decides where the data goes.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// ShutdownFunc flushes and stops all telemetry providers.
type ShutdownFunc func(context.Context) error

// Setup installs global TracerProvider, MeterProvider and W3C propagators.
// serviceName is only a default: OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES win.
func Setup(ctx context.Context, serviceName, serviceVersion string) (ShutdownFunc, error) {
	// OTEL_SDK_DISABLED=true is the spec-defined kill switch (useful in unit tests).
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{}))
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.New(ctx,
		// Defaults first, environment last, so the environment always wins.
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
		),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithProcessRuntimeName(),
		resource.WithProcessRuntimeVersion(),
		resource.WithFromEnv(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	traceExp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	// The SDK reads OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG; default is
	// parentbased_always_on, which keeps the service graph complete.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)

	metricExp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	// Interval comes from OTEL_METRIC_EXPORT_INTERVAL (default 60s).
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	// Go runtime metrics (GC, goroutines, memory) for container-level saturation.
	if err := runtime.Start(runtime.WithMinimumReadMemStatsInterval(15 * time.Second)); err != nil {
		return nil, fmt.Errorf("runtime metrics: %w", err)
	}

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}
