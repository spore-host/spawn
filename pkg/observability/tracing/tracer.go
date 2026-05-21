package tracing

import (
	"context"
	"fmt"
	"log"

	"github.com/spore-host/spawn/pkg/observability"
	"github.com/spore-host/spawn/pkg/observability/tracing/exporters"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracer wraps OpenTelemetry tracer
type Tracer struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

// NewTracer creates a new tracer
func NewTracer(ctx context.Context, config observability.TracingConfig, serviceName, instanceID, region string) (*Tracer, error) {
	if !config.Enabled {
		// Return no-op tracer
		return &Tracer{
			tracer: otel.GetTracerProvider().Tracer("spawn"),
		}, nil
	}

	// Create resource
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion("0.19.0"),
			attribute.String("cloud.provider", "aws"),
			attribute.String("cloud.region", region),
			attribute.String("host.id", instanceID),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create exporter based on config
	var exporter sdktrace.SpanExporter
	switch config.Exporter {
	case "xray":
		exporter, err = exporters.NewXRayExporter(ctx, region)
		if err != nil {
			return nil, fmt.Errorf("failed to create X-Ray exporter: %w", err)
		}
	case "stdout":
		exporter = exporters.NewStdoutExporter()
	default:
		return nil, fmt.Errorf("unsupported exporter: %s", config.Exporter)
	}

	// Create tracer provider
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(config.SamplingRate)),
	)

	// Set global provider
	otel.SetTracerProvider(provider)

	log.Printf("Tracing enabled: exporter=%s, sampling=%.2f", config.Exporter, config.SamplingRate)

	return &Tracer{
		provider: provider,
		tracer:   provider.Tracer("spawn"),
	}, nil
}

// Shutdown flushes and shuts down the tracer
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t.provider == nil {
		return nil
	}
	return t.provider.Shutdown(ctx)
}

// Tracer returns the OpenTelemetry tracer
func (t *Tracer) Tracer() trace.Tracer {
	return t.tracer
}
