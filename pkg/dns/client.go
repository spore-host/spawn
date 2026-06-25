package dns

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

const (
	defaultDomain = "spore.host"

	// sigV4Service is the SigV4 service name for a Lambda Function URL. AWS_IAM
	// auth on a Function URL is verified as the "lambda" service.
	sigV4Service = "lambda"
)

// DNSUpdateRequest represents the request to the DNS API
type DNSUpdateRequest struct {
	InstanceIdentityDocument  string `json:"instance_identity_document"`
	InstanceIdentitySignature string `json:"instance_identity_signature"`
	RecordName                string `json:"record_name"`
	IPAddress                 string `json:"ip_address"`
	Action                    string `json:"action"`
	Domain                    string `json:"domain,omitempty"`
	JobArrayID                string `json:"job_array_id,omitempty"`   // Optional: for group DNS
	JobArrayName              string `json:"job_array_name,omitempty"` // Optional: for group DNS
	AccountName               string `json:"account_name,omitempty"`   // Optional: DNS-safe account-name slug for the alias FQDN (#121)
}

// DNSUpdateResponse represents the response from the DNS API
type DNSUpdateResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	Error     string `json:"error"`
	Record    string `json:"record"`
	ChangeID  string `json:"change_id"`
	Timestamp string `json:"timestamp"`
}

// Client handles DNS API operations
type Client struct {
	httpClient  *http.Client
	imdsClient  *imds.Client
	domain      string
	apiEndpoint string
	accountName string // DNS-safe account-name slug; included in requests for the alias FQDN (#121)

	// SigV4 signing (#173). When sign is true, callAPI SigV4-signs the POST with
	// the instance role's credentials so the DNS Lambda Function URL can run under
	// AuthType: AWS_IAM and derive the *verified* caller account from the signed
	// principal — replacing the spoofable instance-identity-document path. This is
	// gated so the signing client can ship to instances BEFORE the Function URL
	// AuthType is flipped in infra (a signed request against an AuthType: NONE URL
	// is accepted — the auth headers are simply ignored), keeping the rollout
	// non-breaking and lockstep-safe. credsProvider/region feed the signer.
	sign          bool
	credsProvider aws.CredentialsProvider
	region        string
	signer        *v4.Signer
}

// SetAccountName sets the account-name slug included in DNS requests so the
// updater registers the friendly alias FQDN ({record}.{account-name}.{domain}).
// Empty (the default) means base36 only.
func (c *Client) SetAccountName(slug string) { c.accountName = slug }

// NewClient creates a new DNS client with optional custom domain and API endpoint
// If domain or apiEndpoint are empty, defaults are used
func NewClient(ctx context.Context, domain, apiEndpoint string) (*Client, error) {
	// Load AWS config for IMDS
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Use defaults if not provided
	if domain == "" {
		domain = defaultDomain
	}
	if apiEndpoint == "" {
		apiEndpoint = spawnconfig.GetDNSEndpointURL()
	}

	c := &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		imdsClient:  imds.NewFromConfig(cfg),
		domain:      domain,
		apiEndpoint: apiEndpoint,
	}

	// Opt into SigV4 signing (#173) when SPORE_DNS_SIGV4 is set. Sourced from the
	// ambient credential chain (the instance role on EC2), which is exactly the
	// principal the Lambda will authorize once AuthType: AWS_IAM is enabled. We
	// keep this behind a flag so the signing build can roll out to instances
	// ahead of the infra AuthType flip without changing live behavior; the flip
	// and the default-on switch happen in lockstep (see #173 cutover plan).
	if sigV4Enabled() {
		c.sign = true
		c.credsProvider = cfg.Credentials
		c.region = cfg.Region
		if c.region == "" {
			c.region = "us-east-1" // Function URL region; IMDS-less envs may not set it
		}
		c.signer = v4.NewSigner()
	}

	return c, nil
}

// sigV4Enabled reports whether DNS requests should be SigV4-signed (#173).
// Opt-in via SPORE_DNS_SIGV4 (any non-empty value) so the cutover to AWS_IAM
// auth on the Function URL can be coordinated with infra.
func sigV4Enabled() bool {
	return os.Getenv("SPORE_DNS_SIGV4") != ""
}

// RegisterDNS registers a DNS record for the current instance
func (c *Client) RegisterDNS(ctx context.Context, recordName, ipAddress string) (*DNSUpdateResponse, error) {
	// Validate record name
	recordName = strings.ToLower(strings.TrimSpace(recordName))
	validName := regexp.MustCompile(`^[a-z0-9-]+$`)
	if !validName.MatchString(recordName) {
		return nil, fmt.Errorf("invalid DNS name: %s (must be alphanumeric and hyphens only)", recordName)
	}

	// Get instance identity document
	identityDoc, err := c.imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance identity document: %w", err)
	}

	// Marshal identity document to JSON
	identityDocJSON, err := json.Marshal(identityDoc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal identity document: %w", err)
	}

	// Get instance identity signature
	sigResp, err := c.imdsClient.GetDynamicData(ctx, &imds.GetDynamicDataInput{
		Path: "instance-identity/signature",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get identity signature: %w", err)
	}
	defer func() { _ = sigResp.Content.Close() }()

	signatureBytes, err := io.ReadAll(sigResp.Content)
	if err != nil {
		return nil, fmt.Errorf("failed to read signature: %w", err)
	}

	// Build request
	req := DNSUpdateRequest{
		InstanceIdentityDocument:  base64.StdEncoding.EncodeToString(identityDocJSON),
		InstanceIdentitySignature: strings.TrimSpace(string(signatureBytes)),
		RecordName:                recordName,
		IPAddress:                 ipAddress,
		Action:                    "UPSERT",
		Domain:                    c.domain,
		AccountName:               c.accountName,
	}

	return c.callAPI(ctx, req)
}

// RegisterJobArrayDNS registers both per-instance and group DNS for a job array member
func (c *Client) RegisterJobArrayDNS(ctx context.Context, recordName, ipAddress, jobArrayID, jobArrayName string) (*DNSUpdateResponse, error) {
	// Validate record name
	recordName = strings.ToLower(strings.TrimSpace(recordName))
	validName := regexp.MustCompile(`^[a-z0-9.-]+$`)
	if !validName.MatchString(recordName) {
		return nil, fmt.Errorf("invalid DNS name: %s (must be alphanumeric, hyphens, and dots only)", recordName)
	}

	// Get instance identity document
	identityDoc, err := c.imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance identity document: %w", err)
	}

	// Marshal identity document to JSON
	identityDocJSON, err := json.Marshal(identityDoc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal identity document: %w", err)
	}

	// Get instance identity signature
	sigResp, err := c.imdsClient.GetDynamicData(ctx, &imds.GetDynamicDataInput{
		Path: "instance-identity/signature",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get identity signature: %w", err)
	}
	defer func() { _ = sigResp.Content.Close() }()

	signatureBytes, err := io.ReadAll(sigResp.Content)
	if err != nil {
		return nil, fmt.Errorf("failed to read signature: %w", err)
	}

	// Build request with job array fields
	req := DNSUpdateRequest{
		InstanceIdentityDocument:  base64.StdEncoding.EncodeToString(identityDocJSON),
		InstanceIdentitySignature: strings.TrimSpace(string(signatureBytes)),
		RecordName:                recordName,
		IPAddress:                 ipAddress,
		Action:                    "UPSERT",
		Domain:                    c.domain,
		AccountName:               c.accountName,
		JobArrayID:                jobArrayID,
		JobArrayName:              jobArrayName,
	}

	return c.callAPI(ctx, req)
}

// DeleteJobArrayDNS deletes both per-instance and group DNS for a job array member
func (c *Client) DeleteJobArrayDNS(ctx context.Context, recordName, ipAddress, jobArrayID, jobArrayName string) (*DNSUpdateResponse, error) {
	// Get instance identity
	identityDoc, err := c.imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance identity document: %w", err)
	}

	identityDocJSON, err := json.Marshal(identityDoc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal identity document: %w", err)
	}

	sigResp, err := c.imdsClient.GetDynamicData(ctx, &imds.GetDynamicDataInput{
		Path: "instance-identity/signature",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get identity signature: %w", err)
	}
	defer func() { _ = sigResp.Content.Close() }()

	signatureBytes, err := io.ReadAll(sigResp.Content)
	if err != nil {
		return nil, fmt.Errorf("failed to read signature: %w", err)
	}

	req := DNSUpdateRequest{
		InstanceIdentityDocument:  base64.StdEncoding.EncodeToString(identityDocJSON),
		InstanceIdentitySignature: strings.TrimSpace(string(signatureBytes)),
		RecordName:                recordName,
		IPAddress:                 ipAddress,
		Action:                    "DELETE",
		Domain:                    c.domain,
		AccountName:               c.accountName,
		JobArrayID:                jobArrayID,
		JobArrayName:              jobArrayName,
	}

	return c.callAPI(ctx, req)
}

// DeleteDNS deletes a DNS record for the current instance
func (c *Client) DeleteDNS(ctx context.Context, recordName, ipAddress string) (*DNSUpdateResponse, error) {
	// Get instance identity
	identityDoc, err := c.imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance identity document: %w", err)
	}

	identityDocJSON, err := json.Marshal(identityDoc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal identity document: %w", err)
	}

	sigResp, err := c.imdsClient.GetDynamicData(ctx, &imds.GetDynamicDataInput{
		Path: "instance-identity/signature",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get identity signature: %w", err)
	}
	defer func() { _ = sigResp.Content.Close() }()

	signatureBytes, err := io.ReadAll(sigResp.Content)
	if err != nil {
		return nil, fmt.Errorf("failed to read signature: %w", err)
	}

	req := DNSUpdateRequest{
		InstanceIdentityDocument:  base64.StdEncoding.EncodeToString(identityDocJSON),
		InstanceIdentitySignature: strings.TrimSpace(string(signatureBytes)),
		RecordName:                recordName,
		IPAddress:                 ipAddress,
		Action:                    "DELETE",
		Domain:                    c.domain,
		AccountName:               c.accountName,
	}

	return c.callAPI(ctx, req)
}

// callAPI makes the actual HTTP request to the DNS API
func (c *Client) callAPI(ctx context.Context, req DNSUpdateRequest) (*DNSUpdateResponse, error) {
	// Marshal request
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.apiEndpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// SigV4-sign with the instance role creds when enabled (#173), so the DNS
	// Lambda Function URL can authorize the *verified* caller under AuthType:
	// AWS_IAM. No-op (plain POST) when signing is off.
	if err := c.signRequest(ctx, httpReq, reqBody); err != nil {
		return nil, err
	}

	// Make request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to call DNS API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var dnsResp DNSUpdateResponse
	if err := json.Unmarshal(respBody, &dnsResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Check for errors
	if !dnsResp.Success {
		return &dnsResp, fmt.Errorf("DNS API error: %s", dnsResp.Error)
	}

	return &dnsResp, nil
}

// signRequest SigV4-signs httpReq in place using the instance role's
// credentials, for the lambda service in the Function URL's region (#173). It is
// a no-op when signing is disabled, so the same code path serves both the
// pre-cutover (plain POST, AuthType: NONE) and post-cutover (AWS_IAM) worlds.
// The payload hash is the hex SHA-256 of the body, as SignHTTP requires.
func (c *Client) signRequest(ctx context.Context, httpReq *http.Request, body []byte) error {
	if !c.sign || c.signer == nil || c.credsProvider == nil {
		return nil
	}
	creds, err := c.credsProvider.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("retrieve credentials for DNS request signing: %w", err)
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	if err := c.signer.SignHTTP(ctx, creds, httpReq, payloadHash, sigV4Service, c.region, time.Now()); err != nil {
		return fmt.Errorf("sign DNS request: %w", err)
	}
	return nil
}

// GetFQDN returns the fully qualified domain name for a record
func (c *Client) GetFQDN(recordName string) string {
	return fmt.Sprintf("%s.%s", recordName, c.domain)
}

// GetFQDN returns the fully qualified domain name for a record using default domain
// Deprecated: Use Client.GetFQDN() instead for custom domain support
func GetFQDN(recordName string) string {
	return fmt.Sprintf("%s.%s", recordName, defaultDomain)
}
