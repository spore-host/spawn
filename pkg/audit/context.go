package audit

import (
	"context"

	"github.com/google/uuid"
)

type contextKey string

const (
	userIDKey        contextKey = "audit_user_id"
	correlationIDKey contextKey = "audit_correlation_id"
	loggerKey        contextKey = "audit_logger"
)

// NewContextWithAudit creates a new context with audit information.
// It generates a new correlation ID and stores the user ID.
func NewContextWithAudit(ctx context.Context, userID string) context.Context {
	ctx = context.WithValue(ctx, userIDKey, userID)
	correlationID := uuid.New().String()
	ctx = context.WithValue(ctx, correlationIDKey, correlationID)
	return ctx
}

// NewContextWithCorrelationID creates a new context with a specific correlation ID.
// Useful when resuming operations or tracing across systems.
func NewContextWithCorrelationID(ctx context.Context, userID, correlationID string) context.Context {
	ctx = context.WithValue(ctx, userIDKey, userID)
	ctx = context.WithValue(ctx, correlationIDKey, correlationID)
	return ctx
}

// GetUserIDFromContext retrieves the user ID from the context.
// Returns "unknown" if not found.
func GetUserIDFromContext(ctx context.Context) string {
	if v := ctx.Value(userIDKey); v != nil {
		if userID, ok := v.(string); ok {
			return userID
		}
	}
	return "unknown"
}

// GetCorrelationIDFromContext retrieves the correlation ID from the context.
// Returns empty string if not found.
func GetCorrelationIDFromContext(ctx context.Context) string {
	if v := ctx.Value(correlationIDKey); v != nil {
		if correlationID, ok := v.(string); ok {
			return correlationID
		}
	}
	return ""
}

// GetLoggerFromContext retrieves an audit logger from the context.
// If no logger is found, returns nil.
func GetLoggerFromContext(ctx context.Context) *AuditLogger {
	if v := ctx.Value(loggerKey); v != nil {
		if logger, ok := v.(*AuditLogger); ok {
			return logger
		}
	}
	return nil
}

// SetLoggerInContext stores an audit logger in the context.
func SetLoggerInContext(ctx context.Context, logger *AuditLogger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// NewLoggerFromContext creates a new audit logger using context values.
// Falls back to "unknown" user ID if not found in context.
func NewLoggerFromContext(ctx context.Context) *AuditLogger {
	userID := GetUserIDFromContext(ctx)
	correlationID := GetCorrelationIDFromContext(ctx)

	// If no correlation ID in context, generate one
	if correlationID == "" {
		correlationID = uuid.New().String()
	}

	// Check if there's already a logger in context
	if logger := GetLoggerFromContext(ctx); logger != nil {
		return logger
	}

	// Create new logger (defaults to stderr in most cases)
	return NewLogger(nil, userID, correlationID)
}
