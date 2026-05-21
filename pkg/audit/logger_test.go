package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestNewLogger(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, "user123", "corr-456")

	if logger.userID != "user123" {
		t.Errorf("Expected userID user123, got %s", logger.userID)
	}

	if logger.correlationID != "corr-456" {
		t.Errorf("Expected correlationID corr-456, got %s", logger.correlationID)
	}
}

func TestLogOperation(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, "user123", "corr-456")

	logger.LogOperation("launch_instance", "i-1234567890", "success", nil)

	var event AuditEvent
	if err := json.NewDecoder(buf).Decode(&event); err != nil {
		t.Fatalf("Failed to decode audit event: %v", err)
	}

	if event.Operation != "launch_instance" {
		t.Errorf("Expected operation launch_instance, got %s", event.Operation)
	}

	if event.InstanceID != "i-1234567890" {
		t.Errorf("Expected instance ID i-1234567890, got %s", event.InstanceID)
	}

	if event.Result != "success" {
		t.Errorf("Expected result success, got %s", event.Result)
	}

	if event.UserID != "user123" {
		t.Errorf("Expected userID user123, got %s", event.UserID)
	}

	if event.CorrelationID != "corr-456" {
		t.Errorf("Expected correlationID corr-456, got %s", event.CorrelationID)
	}

	if event.Level != "info" {
		t.Errorf("Expected level info, got %s", event.Level)
	}
}

func TestLogOperationWithError(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, "user123", "corr-456")

	testErr := errors.New("permission denied")
	logger.LogOperation("launch_instance", "i-1234567890", "failed", testErr)

	var event AuditEvent
	if err := json.NewDecoder(buf).Decode(&event); err != nil {
		t.Fatalf("Failed to decode audit event: %v", err)
	}

	if event.Level != "error" {
		t.Errorf("Expected level error, got %s", event.Level)
	}

	if event.Error != "permission denied" {
		t.Errorf("Expected error 'permission denied', got %s", event.Error)
	}

	if event.Result != "failed" {
		t.Errorf("Expected result failed, got %s", event.Result)
	}
}

func TestLogOperationWithRegion(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, "user123", "corr-456")

	logger.LogOperationWithRegion("launch_instance", "i-1234567890", "us-east-1", "success", nil)

	var event AuditEvent
	if err := json.NewDecoder(buf).Decode(&event); err != nil {
		t.Fatalf("Failed to decode audit event: %v", err)
	}

	if event.Region != "us-east-1" {
		t.Errorf("Expected region us-east-1, got %s", event.Region)
	}
}

func TestLogOperationWithData(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, "user123", "corr-456")

	data := map[string]interface{}{
		"instance_type": "t3.micro",
		"count":         2,
	}

	logger.LogOperationWithData("launch_instance", "sweep-123", "success", data, nil)

	var event AuditEvent
	if err := json.NewDecoder(buf).Decode(&event); err != nil {
		t.Fatalf("Failed to decode audit event: %v", err)
	}

	if event.AdditionalData["instance_type"] != "t3.micro" {
		t.Errorf("Expected instance_type t3.micro, got %v", event.AdditionalData["instance_type"])
	}

	// Note: JSON numbers are float64
	if event.AdditionalData["count"] != float64(2) {
		t.Errorf("Expected count 2, got %v", event.AdditionalData["count"])
	}
}

func TestNewLoggerWithNilWriter(t *testing.T) {
	logger := NewLogger(nil, "user123", "corr-456")

	// Should not panic
	logger.LogOperation("test", "resource", "success", nil)
}

func TestGettersSetters(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, "user123", "corr-456")

	logger.SetUserID("newuser")
	if logger.userID != "newuser" {
		t.Errorf("Expected userID newuser, got %s", logger.userID)
	}

	logger.SetCorrelationID("new-corr")
	if logger.correlationID != "new-corr" {
		t.Errorf("Expected correlationID new-corr, got %s", logger.correlationID)
	}

	if logger.GetCorrelationID() != "new-corr" {
		t.Errorf("Expected GetCorrelationID to return new-corr, got %s", logger.GetCorrelationID())
	}
}

func TestNewContextWithAudit(t *testing.T) {
	ctx := context.Background()
	ctx = NewContextWithAudit(ctx, "user123")

	userID := GetUserIDFromContext(ctx)
	if userID != "user123" {
		t.Errorf("Expected userID user123, got %s", userID)
	}

	correlationID := GetCorrelationIDFromContext(ctx)
	if correlationID == "" {
		t.Error("Expected non-empty correlation ID")
	}
}

func TestGetUserIDFromContextUnknown(t *testing.T) {
	ctx := context.Background()
	userID := GetUserIDFromContext(ctx)

	if userID != "unknown" {
		t.Errorf("Expected userID unknown, got %s", userID)
	}
}

func TestNewLoggerFromContext(t *testing.T) {
	ctx := context.Background()
	ctx = NewContextWithAudit(ctx, "user456")

	logger := NewLoggerFromContext(ctx)

	if logger.userID != "user456" {
		t.Errorf("Expected userID user456, got %s", logger.userID)
	}

	if logger.correlationID == "" {
		t.Error("Expected non-empty correlation ID")
	}
}

func TestSetLoggerInContext(t *testing.T) {
	ctx := context.Background()
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, "user123", "corr-456")

	ctx = SetLoggerInContext(ctx, logger)

	retrievedLogger := GetLoggerFromContext(ctx)
	if retrievedLogger == nil {
		t.Fatal("Expected logger from context, got nil")
	}

	if retrievedLogger.userID != "user123" {
		t.Errorf("Expected userID user123, got %s", retrievedLogger.userID)
	}
}

// TestAuditLogFormat ensures audit logs are properly formatted JSON
func TestAuditLogFormat(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, "user123", "corr-456")

	logger.LogOperation("test_operation", "resource-123", "success", nil)

	output := buf.String()

	// Should be valid JSON
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(output), &event); err != nil {
		t.Fatalf("Audit log is not valid JSON: %v", err)
	}

	// Should contain required fields
	requiredFields := []string{"timestamp", "level", "operation", "result"}
	for _, field := range requiredFields {
		if _, ok := event[field]; !ok {
			t.Errorf("Audit log missing required field: %s", field)
		}
	}
}

// ExampleAuditLogger demonstrates basic usage
func ExampleAuditLogger() {
	logger := NewLogger(os.Stdout, "user123", "request-abc")
	logger.LogOperation("launch_instance", "i-1234567890", "success", nil)
}

// ExampleAuditLogger_withError demonstrates error logging
func ExampleAuditLogger_withError() {
	logger := NewLogger(os.Stdout, "user123", "request-abc")
	err := errors.New("insufficient capacity")
	logger.LogOperation("launch_instance", "i-1234567890", "failed", err)
}
