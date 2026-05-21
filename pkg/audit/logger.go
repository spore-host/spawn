package audit

import (
	"encoding/json"
	"io"
	"time"
)

// AuditLogger handles structured audit logging for security-sensitive operations.
type AuditLogger struct {
	writer        io.Writer
	userID        string
	correlationID string
}

// AuditEvent represents a single audit log entry.
type AuditEvent struct {
	Timestamp      time.Time              `json:"timestamp"`
	Level          string                 `json:"level"`
	Operation      string                 `json:"operation"`
	UserID         string                 `json:"user_id,omitempty"`
	InstanceID     string                 `json:"instance_id,omitempty"`
	Region         string                 `json:"region,omitempty"`
	CorrelationID  string                 `json:"correlation_id,omitempty"`
	Result         string                 `json:"result"`
	Error          string                 `json:"error,omitempty"`
	AdditionalData map[string]interface{} `json:"additional_data,omitempty"`
}

// NewLogger creates a new audit logger.
func NewLogger(writer io.Writer, userID, correlationID string) *AuditLogger {
	if writer == nil {
		// Default to no-op if no writer provided
		writer = io.Discard
	}

	return &AuditLogger{
		writer:        writer,
		userID:        userID,
		correlationID: correlationID,
	}
}

// LogOperation logs an audit event for an operation.
func (l *AuditLogger) LogOperation(operation, resourceID, result string, err error) {
	event := AuditEvent{
		Timestamp:     time.Now().UTC(),
		Level:         "info",
		Operation:     operation,
		UserID:        l.userID,
		InstanceID:    resourceID,
		CorrelationID: l.correlationID,
		Result:        result,
	}

	if err != nil {
		event.Level = "error"
		event.Error = err.Error()
	}

	// Write as JSON
	_ = json.NewEncoder(l.writer).Encode(event)
}

// LogOperationWithRegion logs an audit event with region information.
func (l *AuditLogger) LogOperationWithRegion(operation, resourceID, region, result string, err error) {
	event := AuditEvent{
		Timestamp:     time.Now().UTC(),
		Level:         "info",
		Operation:     operation,
		UserID:        l.userID,
		InstanceID:    resourceID,
		Region:        region,
		CorrelationID: l.correlationID,
		Result:        result,
	}

	if err != nil {
		event.Level = "error"
		event.Error = err.Error()
	}

	// Write as JSON
	_ = json.NewEncoder(l.writer).Encode(event)
}

// LogOperationWithData logs an audit event with additional structured data.
func (l *AuditLogger) LogOperationWithData(operation, resourceID, result string, data map[string]interface{}, err error) {
	event := AuditEvent{
		Timestamp:      time.Now().UTC(),
		Level:          "info",
		Operation:      operation,
		UserID:         l.userID,
		InstanceID:     resourceID,
		CorrelationID:  l.correlationID,
		Result:         result,
		AdditionalData: data,
	}

	if err != nil {
		event.Level = "error"
		event.Error = err.Error()
	}

	// Write as JSON
	_ = json.NewEncoder(l.writer).Encode(event)
}

// SetUserID updates the user ID for subsequent log entries.
func (l *AuditLogger) SetUserID(userID string) {
	l.userID = userID
}

// SetCorrelationID updates the correlation ID for subsequent log entries.
func (l *AuditLogger) SetCorrelationID(correlationID string) {
	l.correlationID = correlationID
}

// GetCorrelationID returns the current correlation ID.
func (l *AuditLogger) GetCorrelationID() string {
	return l.correlationID
}
