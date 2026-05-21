package exporters

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/xray"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// XRayExporter exports traces to AWS X-Ray
type XRayExporter struct {
	client *xray.Client
	region string
}

// NewXRayExporter creates a new X-Ray exporter
func NewXRayExporter(ctx context.Context, region string) (*XRayExporter, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &XRayExporter{
		client: xray.NewFromConfig(cfg),
		region: region,
	}, nil
}

// ExportSpans exports spans to X-Ray
func (e *XRayExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}

	// Convert OpenTelemetry spans to X-Ray segments
	documents := make([]string, 0, len(spans))

	for _, span := range spans {
		doc, err := e.convertSpanToDocument(span)
		if err != nil {
			log.Printf("Warning: Failed to convert span to X-Ray document: %v", err)
			continue
		}
		documents = append(documents, doc)
	}

	if len(documents) == 0 {
		return nil
	}

	// Send to X-Ray
	_, err := e.client.PutTraceSegments(ctx, &xray.PutTraceSegmentsInput{
		TraceSegmentDocuments: documents,
	})

	if err != nil {
		return fmt.Errorf("failed to put trace segments: %w", err)
	}

	return nil
}

// Shutdown shuts down the exporter
func (e *XRayExporter) Shutdown(ctx context.Context) error {
	return nil
}

// convertSpanToDocument converts an OpenTelemetry span to X-Ray document format
func (e *XRayExporter) convertSpanToDocument(span sdktrace.ReadOnlySpan) (string, error) {
	// Extract trace ID and span ID
	traceID := span.SpanContext().TraceID().String()
	spanID := span.SpanContext().SpanID().String()

	// Build X-Ray segment
	segment := map[string]interface{}{
		"trace_id":   fmt.Sprintf("1-%s-%s", traceID[:8], traceID[8:]),
		"id":         spanID,
		"name":       span.Name(),
		"start_time": float64(span.StartTime().UnixNano()) / 1e9,
		"end_time":   float64(span.EndTime().UnixNano()) / 1e9,
		"service": map[string]string{
			"name": "spawn",
		},
	}

	// Add attributes
	if attrs := span.Attributes(); len(attrs) > 0 {
		metadata := make(map[string]interface{})
		for _, attr := range attrs {
			metadata[string(attr.Key)] = attr.Value.AsInterface()
		}
		segment["metadata"] = map[string]interface{}{
			"spawn": metadata,
		}
	}

	// Marshal to JSON
	data, err := json.Marshal(segment)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// MarshalLog implements log marshaling (no-op for security)
func (e *XRayExporter) MarshalLog() interface{} {
	return struct {
		Type   string
		Region string
	}{
		Type:   "xray",
		Region: e.region,
	}
}
