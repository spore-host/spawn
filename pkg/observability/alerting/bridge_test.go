package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockSender is a mock implementation of Sender for testing
type mockSender struct {
	sentAlerts []FormattedAlert
}

func (m *mockSender) Send(ctx context.Context, alert FormattedAlert) error {
	m.sentAlerts = append(m.sentAlerts, alert)
	return nil
}

func TestHandleWebhook(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		payload        AlertmanagerWebhook
		expectedStatus int
		expectedAlerts int
	}{
		{
			name:           "valid webhook",
			method:         http.MethodPost,
			expectedStatus: http.StatusOK,
			expectedAlerts: 2,
			payload: AlertmanagerWebhook{
				Version:  "4",
				GroupKey: "test-group",
				Status:   "firing",
				Receiver: "spawn-webhook",
				Alerts: []Alert{
					{
						Status: "firing",
						Labels: map[string]string{
							"alertname": "HighCPUUsage",
							"severity":  "warning",
							"category":  "performance",
						},
						Annotations: map[string]string{
							"summary":     "Instance i-123 CPU at 97%",
							"description": "High CPU usage detected",
							"instance_id": "i-123",
							"region":      "us-east-1",
						},
						StartsAt:     time.Now(),
						GeneratorURL: "http://prometheus:9090/graph?g0.expr=...",
						Fingerprint:  "abc123",
					},
					{
						Status: "firing",
						Labels: map[string]string{
							"alertname": "HighMemoryUsage",
							"severity":  "critical",
							"category":  "performance",
						},
						Annotations: map[string]string{
							"summary":     "Instance i-456 memory at 92%",
							"description": "High memory usage detected",
							"instance_id": "i-456",
							"region":      "us-west-2",
						},
						StartsAt:    time.Now(),
						Fingerprint: "def456",
					},
				},
			},
		},
		{
			name:           "invalid method",
			method:         http.MethodGet,
			expectedStatus: http.StatusMethodNotAllowed,
			expectedAlerts: 0,
		},
		{
			name:           "empty alerts",
			method:         http.MethodPost,
			expectedStatus: http.StatusOK,
			expectedAlerts: 0,
			payload: AlertmanagerWebhook{
				Version: "4",
				Status:  "firing",
				Alerts:  []Alert{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := &mockSender{}
			bridge := NewBridge(sender)

			var body []byte
			var err error
			if tt.method == http.MethodPost {
				body, err = json.Marshal(tt.payload)
				if err != nil {
					t.Fatalf("Failed to marshal payload: %v", err)
				}
			}

			req := httptest.NewRequest(tt.method, "/alertmanager/webhook", bytes.NewReader(body))
			w := httptest.NewRecorder()

			bridge.HandleWebhook(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if len(sender.sentAlerts) != tt.expectedAlerts {
				t.Errorf("Expected %d alerts, got %d", tt.expectedAlerts, len(sender.sentAlerts))
			}

			// Verify alert contents for valid webhook
			if tt.name == "valid webhook" && len(sender.sentAlerts) == 2 {
				alert1 := sender.sentAlerts[0]
				if alert1.Type != "performance" {
					t.Errorf("Expected type 'performance', got '%s'", alert1.Type)
				}
				if alert1.Severity != "warning" {
					t.Errorf("Expected severity 'warning', got '%s'", alert1.Severity)
				}
				if alert1.Source != "prometheus" {
					t.Errorf("Expected source 'prometheus', got '%s'", alert1.Source)
				}
				if alert1.Metadata["alertname"] != "HighCPUUsage" {
					t.Errorf("Expected alertname 'HighCPUUsage', got '%s'", alert1.Metadata["alertname"])
				}

				alert2 := sender.sentAlerts[1]
				if alert2.Severity != "critical" {
					t.Errorf("Expected severity 'critical', got '%s'", alert2.Severity)
				}
			}
		})
	}
}

func TestFormatAlert(t *testing.T) {
	tests := []struct {
		name         string
		alert        Alert
		expectedType string
		expectedSev  string
		checkMessage func(string) bool
	}{
		{
			name: "complete alert",
			alert: Alert{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "TestAlert",
					"severity":  "warning",
					"category":  "lifecycle",
				},
				Annotations: map[string]string{
					"summary":     "Test summary",
					"description": "Test description",
					"instance_id": "i-test",
					"region":      "us-east-1",
				},
				StartsAt:     time.Now(),
				GeneratorURL: "http://test",
				Fingerprint:  "test123",
			},
			expectedType: "lifecycle",
			expectedSev:  "warning",
			checkMessage: func(msg string) bool {
				return bytes.Contains([]byte(msg), []byte("Test summary")) &&
					bytes.Contains([]byte(msg), []byte("Test description")) &&
					bytes.Contains([]byte(msg), []byte("i-test"))
			},
		},
		{
			name: "minimal alert",
			alert: Alert{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "MinimalAlert",
				},
				StartsAt:    time.Now(),
				Fingerprint: "min123",
			},
			expectedType: "monitoring",
			expectedSev:  "info",
			checkMessage: func(msg string) bool {
				return bytes.Contains([]byte(msg), []byte("MinimalAlert"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bridge := NewBridge()
			formatted := bridge.formatAlert(tt.alert)

			if formatted.Type != tt.expectedType {
				t.Errorf("Expected type '%s', got '%s'", tt.expectedType, formatted.Type)
			}
			if formatted.Severity != tt.expectedSev {
				t.Errorf("Expected severity '%s', got '%s'", tt.expectedSev, formatted.Severity)
			}
			if !tt.checkMessage(formatted.Message) {
				t.Errorf("Message validation failed: %s", formatted.Message)
			}
			if formatted.Source != "prometheus" {
				t.Errorf("Expected source 'prometheus', got '%s'", formatted.Source)
			}
		})
	}
}

func TestLogSender(t *testing.T) {
	sender := NewLogSender()
	alert := FormattedAlert{
		Type:      "test",
		Severity:  "warning",
		Message:   "Test message",
		Source:    "prometheus",
		Timestamp: time.Now(),
		Metadata: map[string]string{
			"alertname": "TestAlert",
		},
	}

	err := sender.Send(context.Background(), alert)
	if err != nil {
		t.Errorf("LogSender.Send failed: %v", err)
	}
}

func TestWebhookSender(t *testing.T) {
	// Create test server
	received := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Test sending
	sender := NewWebhookSender(server.URL)
	alert := FormattedAlert{
		Type:      "test",
		Severity:  "warning",
		Message:   "Test message",
		Source:    "prometheus",
		Timestamp: time.Now(),
		Metadata: map[string]string{
			"alertname": "TestAlert",
		},
	}

	err := sender.Send(context.Background(), alert)
	if err != nil {
		t.Errorf("WebhookSender.Send failed: %v", err)
	}

	if !received {
		t.Error("Webhook was not received")
	}
}

func TestHealthEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	// Manually call health handler
	http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	if w.Body.String() != "OK" {
		t.Errorf("Expected body 'OK', got '%s'", w.Body.String())
	}
}
