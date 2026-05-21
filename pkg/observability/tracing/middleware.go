package tracing

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws"
)

// InstrumentAWSConfig adds OpenTelemetry instrumentation to AWS SDK config
func InstrumentAWSConfig(cfg *aws.Config) {
	otelaws.AppendMiddlewares(&cfg.APIOptions)
}

// StartSpan starts a new span with the given name
func StartSpan(ctx context.Context, tracer *Tracer, name string) (context.Context, func()) {
	if tracer == nil || tracer.tracer == nil {
		return ctx, func() {}
	}

	ctx, span := tracer.tracer.Start(ctx, name)
	return ctx, func() {
		span.End()
	}
}
