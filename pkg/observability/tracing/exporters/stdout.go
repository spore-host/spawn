package exporters

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// StdoutExporter exports traces to stdout (for debugging)
type StdoutExporter struct{}

// NewStdoutExporter creates a new stdout exporter
func NewStdoutExporter() *StdoutExporter {
	return &StdoutExporter{}
}

// ExportSpans exports spans to stdout
func (e *StdoutExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, span := range spans {
		data := map[string]interface{}{
			"trace_id": span.SpanContext().TraceID().String(),
			"span_id":  span.SpanContext().SpanID().String(),
			"name":     span.Name(),
			"start":    span.StartTime(),
			"end":      span.EndTime(),
			"duration": span.EndTime().Sub(span.StartTime()).String(),
			"status":   span.Status().Code.String(),
		}

		// Add attributes
		attrs := make(map[string]interface{})
		for _, attr := range span.Attributes() {
			attrs[string(attr.Key)] = attr.Value.AsInterface()
		}
		if len(attrs) > 0 {
			data["attributes"] = attrs
		}

		// Marshal and print
		jsonData, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "[TRACE] %s\n", jsonData)
	}

	return nil
}

// Shutdown shuts down the exporter
func (e *StdoutExporter) Shutdown(ctx context.Context) error {
	return nil
}
