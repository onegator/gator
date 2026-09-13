// Package telemetry configures OpenTelemetry tracing and metrics. Export goes to an OTLP
// endpoint when OTEL_EXPORTER_OTLP_ENDPOINT is set; otherwise spans and metrics stay
// in-process (no-op exporters) so instrumentation code never branches on config.
package telemetry

import (
	"context"
	"errors"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Setup installs global tracer and meter providers. The returned function flushes and
// shuts them down; call it on exit.
func Setup(ctx context.Context, service, version string) (func(context.Context) error, error) {
	// Merge without a schema URL: resource.Default() carries the SDK's semconv version and
	// pinning another one here conflicts at startup.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(service), semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, err
	}
	var shutdowns []func(context.Context) error

	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res))
	mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		te, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, err
		}
		tp.RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(te))
		me, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return nil, err
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(30*time.Second))))
	}
	mp := sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	shutdowns = append(shutdowns, tp.Shutdown, mp.Shutdown)

	return func(ctx context.Context) error {
		var errs []error
		for _, s := range shutdowns {
			errs = append(errs, s(ctx))
		}
		return errors.Join(errs...)
	}, nil
}

// Tracer returns the gator tracer.
func Tracer() trace.Tracer { return otel.Tracer("github.com/onegator/gator") }

// Meter returns the gator meter.
func Meter() metric.Meter { return otel.Meter("github.com/onegator/gator") }
