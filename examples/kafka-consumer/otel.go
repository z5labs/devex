package main

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// setupOTel wires the three OpenTelemetry signals over OTLP/gRPC and installs
// them as the global providers. All three exporters read the standard OTLP
// environment (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_INSECURE), and
// the resource picks up OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES, so the
// harness (and any real deployment) configures export entirely via env.
//
// The returned shutdown flushes and stops every provider; callers must run it
// before the process exits or the bounded run loses its telemetry.
func setupOTel(ctx context.Context) (shutdown func(context.Context) error, tp *sdktrace.TracerProvider, mp *sdkmetric.MeterProvider, err error) {
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, nil, nil, err
	}

	traceExp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)

	metricExp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	mp = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	logExp, err := otlploggrpc.New(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	logglobal.SetLoggerProvider(lp)

	shutdown = func(ctx context.Context) error {
		return errors.Join(
			tp.ForceFlush(ctx), mp.ForceFlush(ctx), lp.ForceFlush(ctx),
			tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx),
		)
	}
	return shutdown, tp, mp, nil
}
