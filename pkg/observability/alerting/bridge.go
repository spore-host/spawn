package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AlertmanagerWebhook represents the payload from Alertmanager
type AlertmanagerWebhook struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"` // "firing" or "resolved"
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert represents a single alert in the Alertmanager webhook
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Sender is an interface for sending alerts to external systems
type Sender interface {
	Send(ctx context.Context, alert FormattedAlert) error
}

// FormattedAlert is a simplified alert format for sending
type FormattedAlert struct {
	Type      string
	Severity  string
	Message   string
	Source    string
	Timestamp time.Time
	Metadata  map[string]string
}

// Bridge handles Alertmanager webhooks and forwards them to external systems
type Bridge struct {
	senders []Sender
}

// NewBridge creates a new Alertmanager webhook bridge
func NewBridge(senders ...Sender) *Bridge {
	return &Bridge{
		senders: senders,
	}
}

// HandleWebhook processes incoming Alertmanager webhook requests
func (b *Bridge) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	var webhook AlertmanagerWebhook
	if err := json.Unmarshal(body, &webhook); err != nil {
		http.Error(w, "Failed to parse webhook payload", http.StatusBadRequest)
		return
	}

	// Convert and send each alert
	for _, alert := range webhook.Alerts {
		formattedAlert := b.formatAlert(alert)
		for _, sender := range b.senders {
			if err := sender.Send(r.Context(), formattedAlert); err != nil {
				// Log error but don't fail the request
				fmt.Printf("Failed to send alert %s: %v\n", alert.Labels["alertname"], err)
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// formatAlert converts an Alertmanager alert to FormattedAlert
func (b *Bridge) formatAlert(alert Alert) FormattedAlert {
	// Determine severity from labels
	severity := alert.Labels["severity"]
	if severity == "" {
		severity = "info"
	}

	// Build alert message
	message := alert.Annotations["summary"]
	if message == "" {
		message = fmt.Sprintf("Alert: %s", alert.Labels["alertname"])
	}

	// Add description if present
	if desc := alert.Annotations["description"]; desc != "" {
		message = fmt.Sprintf("%s\n\n%s", message, desc)
	}

	// Add instance information if present
	if instanceID := alert.Annotations["instance_id"]; instanceID != "" {
		message = fmt.Sprintf("%s\n\nInstance: %s", message, instanceID)
	}
	if region := alert.Annotations["region"]; region != "" {
		message = fmt.Sprintf("%s\nRegion: %s", message, region)
	}

	// Add alert URL
	if alert.GeneratorURL != "" {
		message = fmt.Sprintf("%s\n\nDetails: %s", message, alert.GeneratorURL)
	}

	// Determine alert type based on category
	alertType := "monitoring"
	if category := alert.Labels["category"]; category != "" {
		alertType = category
	}

	return FormattedAlert{
		Type:      alertType,
		Severity:  severity,
		Message:   message,
		Source:    "prometheus",
		Timestamp: alert.StartsAt,
		Metadata: map[string]string{
			"alertname":   alert.Labels["alertname"],
			"fingerprint": alert.Fingerprint,
			"status":      alert.Status,
		},
	}
}

// Start runs the webhook HTTP server
func (b *Bridge) Start(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/alertmanager/webhook", b.HandleWebhook)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("webhook server failed: %w", err)
	}

	return nil
}

// WebhookSender sends alerts to a webhook URL
type WebhookSender struct {
	url    string
	client *http.Client
}

// NewWebhookSender creates a new webhook sender
func NewWebhookSender(url string) *WebhookSender {
	return &WebhookSender{
		url: url,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Send sends an alert to the configured webhook URL
func (w *WebhookSender) Send(ctx context.Context, alert FormattedAlert) error {
	payload, err := json.Marshal(alert)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("send webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}

// LogSender logs alerts to stdout (for testing/development)
type LogSender struct{}

// NewLogSender creates a new log sender
func NewLogSender() *LogSender {
	return &LogSender{}
}

// Send logs an alert to stdout
func (l *LogSender) Send(ctx context.Context, alert FormattedAlert) error {
	fmt.Printf("[%s] %s: %s - %s\n", alert.Timestamp.Format(time.RFC3339), alert.Severity, alert.Type, alert.Message)
	return nil
}
