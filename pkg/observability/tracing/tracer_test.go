package tracing

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/observability"
)

func TestNewTracer_Disabled(t *testing.T) {
	ctx := context.Background()
	config := observability.TracingConfig{
		Enabled: false,
	}

	tracer, err := NewTracer(ctx, config, "test-service", "i-test123", "us-east-1")
	if err != nil {
		t.Fatalf("Failed to create disabled tracer: %v", err)
	}

	if tracer == nil {
		t.Fatal("Expected non-nil tracer")
	}

	if tracer.tracer == nil {
		t.Fatal("Expected non-nil internal tracer")
	}

	// Shutdown should not error
	if err := tracer.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}
}

func TestNewTracer_Stdout(t *testing.T) {
	ctx := context.Background()
	config := observability.TracingConfig{
		Enabled:      true,
		Exporter:     "stdout",
		SamplingRate: 1.0,
	}

	tracer, err := NewTracer(ctx, config, "test-service", "i-test123", "us-east-1")
	if err != nil {
		t.Fatalf("Failed to create stdout tracer: %v", err)
	}

	if tracer.provider == nil {
		t.Fatal("Expected non-nil provider")
	}

	// Create a test span
	ctx, span := tracer.Tracer().Start(ctx, "test-operation")
	span.End()

	// Shutdown
	if err := tracer.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}
}

func TestNewTracer_InvalidExporter(t *testing.T) {
	ctx := context.Background()
	config := observability.TracingConfig{
		Enabled:  true,
		Exporter: "invalid",
	}

	_, err := NewTracer(ctx, config, "test-service", "i-test123", "us-east-1")
	if err == nil {
		t.Error("Expected error for invalid exporter")
	}
}

func TestStartSpan(t *testing.T) {
	ctx := context.Background()
	config := observability.TracingConfig{
		Enabled:      true,
		Exporter:     "stdout",
		SamplingRate: 1.0,
	}

	tracer, err := NewTracer(ctx, config, "test-service", "i-test123", "us-east-1")
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer func() { _ = tracer.Shutdown(ctx) }()

	// Start span
	ctx, end := StartSpan(ctx, tracer, "test-span")
	defer end()

	// Verify context is modified
	if ctx == context.Background() {
		t.Error("Expected context to be modified")
	}
}

func TestStartSpan_NilTracer(t *testing.T) {
	ctx := context.Background()

	// Should not panic with nil tracer
	newCtx, end := StartSpan(ctx, nil, "test-span")
	defer end()

	if newCtx != ctx {
		t.Error("Expected context to remain unchanged with nil tracer")
	}
}
