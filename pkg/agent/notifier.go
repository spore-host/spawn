package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/spore-host/spawn/pkg/provider"
)

// notifyRequest mirrors the NotifyRequest struct in the spore-bot Lambda.
type notifyRequest struct {
	PKCS7                     string `json:"pkcs7,omitempty"`
	InstanceIdentityDocument  string `json:"instance_identity_document,omitempty"`
	InstanceIdentitySignature string `json:"instance_identity_signature,omitempty"`
	Platform                  string `json:"platform"`
	WorkspaceID               string `json:"workspace_id"`
	Command                   string `json:"command,omitempty"` // e.g. "/spore" — routes to correct app config
	EventType                 string `json:"event_type"`
	InstanceName              string `json:"instance_name"`
	InstanceID                string `json:"instance_id"`
	Region                    string `json:"region"`
	DNSName                   string `json:"dns_name,omitempty"`
	Detail                    string `json:"detail,omitempty"`
	// OriginalInstanceID is set only for spot_replacement_retired (#995,
	// #1007): the instance a Spot replacement was launched for. The bot DMs
	// the users registered for that instance, since nobody registers the
	// replacement's own id.
	OriginalInstanceID string `json:"original_instance_id,omitempty"`
}

// Notifier sends lifecycle event notifications to the spore-bot Lambda.
// All calls are fire-and-forget — a slow or unavailable endpoint never
// delays or cancels a lifecycle action.
type Notifier struct {
	notifyURL    string
	workspaceID  string
	platform     string
	command      string // e.g. "/spore" — routes to correct workspace config
	instanceID   string
	instanceName string
	region       string
	dnsName      string
	// originalInstanceID, when set, is sent as original_instance_id; see
	// WithOriginalInstance.
	originalInstanceID string
	imdsClient         *imds.Client
	httpClient         *http.Client
}

// NewNotifier creates a Notifier from the agent config and provider identity.
// Returns nil if NotifyURL is empty — nil Notifier is safe to call (no-op).
func NewNotifier(cfg *provider.Config, identity *provider.Identity) *Notifier {
	if cfg.NotifyURL == "" {
		return nil
	}
	name := cfg.DNSName
	if name == "" {
		name = identity.InstanceID
	}
	// Build full FQDN for the URL field in notifications.
	// spawn:dns-name = "notify-dm-test", spawn:account-base36 = "5k0zfnmq"
	// → fqdn = "notify-dm-test.5k0zfnmq.spore.host"
	fqdn := cfg.DNSName
	if fqdn != "" && cfg.AccountBase36 != "" {
		fqdn = cfg.DNSName + "." + cfg.AccountBase36 + ".spore.host"
	}
	command := cfg.NotifyCommand
	if command == "" {
		command = "/spore" // default for spawn CLI launches
	}
	platform := cfg.NotifyPlatform
	if platform == "" {
		platform = "slack" // back-compat default when no platform tag is set
	}
	return &Notifier{
		notifyURL:    strings.TrimRight(cfg.NotifyURL, "/"),
		workspaceID:  cfg.SlackWorkspaceID,
		platform:     platform,
		command:      command,
		instanceID:   identity.InstanceID,
		instanceName: name,
		region:       identity.Region,
		dnsName:      fqdn,
		imdsClient:   imds.NewFromConfig(awsconfig.Config{}), // default IMDS via link-local
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}
}

// WithOriginalInstance returns a copy of n whose notifications name id as the
// instance they are about (original_instance_id). Used by a Spot replacement,
// whose own instance id nobody registered with the bot (#1007). Nil-safe.
func (n *Notifier) WithOriginalInstance(id string) *Notifier {
	if n == nil {
		return nil
	}
	c := *n
	c.originalInstanceID = id
	return &c
}

// Notify sends a lifecycle event notification asynchronously.
// Safe to call on a nil Notifier.
func (n *Notifier) Notify(ctx context.Context, eventType, detail string) {
	if n == nil || n.notifyURL == "" {
		return
	}
	// Fire-and-forget — use a fresh context so the goroutine isn't cancelled
	// when the calling lifecycle function's context is cancelled at shutdown.
	go func() {
		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := n.send(sendCtx, eventType, detail); err != nil {
			log.Printf("spore-bot notify (%s) failed: %v", eventType, err)
		}
	}()
}

// request builds the /notify body.
func (n *Notifier) request(eventType, detail, pkcs7 string) notifyRequest {
	return notifyRequest{
		PKCS7:              pkcs7,
		Command:            n.command,
		Platform:           n.platform,
		WorkspaceID:        n.workspaceID,
		EventType:          eventType,
		InstanceName:       n.instanceName,
		InstanceID:         n.instanceID,
		Region:             n.region,
		DNSName:            n.dnsName,
		Detail:             detail,
		OriginalInstanceID: n.originalInstanceID,
	}
}

func (n *Notifier) send(ctx context.Context, eventType, detail string) error {
	// Use the PKCS#7 endpoint — it includes the document, certificate, and signature
	// in one self-contained envelope. No hardcoded AWS certificates needed.
	pkcs7Resp, err := n.imdsClient.GetDynamicData(ctx, &imds.GetDynamicDataInput{
		Path: "instance-identity/pkcs7",
	})
	if err != nil {
		return fmt.Errorf("get pkcs7: %w", err)
	}
	defer pkcs7Resp.Content.Close()
	pkcs7Raw, err := io.ReadAll(pkcs7Resp.Content)
	if err != nil {
		return fmt.Errorf("read pkcs7: %w", err)
	}

	// IMDS returns base64-encoded DER; decode to get raw DER before re-encoding for transport.
	pkcs7DER, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(pkcs7Raw)))
	if decErr != nil {
		pkcs7DER = pkcs7Raw // fallback: use as-is if not base64
	}

	body, err := json.Marshal(n.request(eventType, detail, base64.StdEncoding.EncodeToString(pkcs7DER)))
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", n.notifyURL+"/notify", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("notify endpoint returned %d", resp.StatusCode)
	}
	return nil
}
