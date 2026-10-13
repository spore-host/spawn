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
	"net/url"
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
	// Lease asks the updater to stamp a renewal lease beside the record (see
	// lease.go). Sent ONLY by a client that will keep renewing — set via
	// SetRenewing — because a lease that is never renewed expires its record.
	Lease bool `json:"lease,omitempty"`
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
	// renewing is whether this client's owner will re-register on a timer, and
	// therefore whether a registration may carry a lease. False by default and set
	// only by spored's agent: `spawn dns register` uses this same client for a
	// one-shot registration, and a lease it never renewed would expire a live
	// record LeaseMaxAge later.
	renewing bool

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

// SetRenewing declares that the caller will re-register at LeaseRefreshInterval
// for as long as the record should exist, so registrations may carry a lease.
// Only call this from something that actually renews — see the renewing field.
func (c *Client) SetRenewing(renewing bool) { c.renewing = renewing }

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

	// SigV4-sign DNS requests (#173), by DEFAULT (#883). Credentials come from the
	// ambient chain — the instance role on EC2 — which is the principal the
	// AuthType: AWS_IAM Function URL authorizes.
	//
	// This used to be opt-in via SPORE_DNS_SIGV4, so a signing client could ship
	// before the Function URL was flipped to AWS_IAM; the comment here said "the
	// flip and the default-on switch happen in lockstep". The flip happened and the
	// default-on switch never did. It only looked done because the three Linux
	// systemd units set the variable — the Windows service sets no environment, so
	// Windows spored sent UNSIGNED requests, which an AWS_IAM URL refuses with 403
	// whatever its resource policy says (observed on a v0.128.0 Windows spore).
	if sigV4Enabled() {
		c.sign = true
		c.credsProvider = cfg.Credentials
		c.region = signingRegion(apiEndpoint, cfg.Region)
		c.signer = v4.NewSigner()
	}

	return c, nil
}

// sigV4Enabled reports whether DNS requests are SigV4-signed: always, unless
// SPORE_DNS_SIGV4 explicitly opts out (0/false/off/no) for a deployment that
// genuinely runs an unauthenticated DNS endpoint (#883). An unset variable SIGNS —
// that is the change; an unsigned request to an AWS_IAM URL can only be refused.
func sigV4Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SPORE_DNS_SIGV4"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// functionURLHost matches a Lambda Function URL hostname and captures its region.
var functionURLHost = regexp.MustCompile(`^[a-z0-9]+\.lambda-url\.([a-z0-9-]+)\.on\.aws$`)

// signingRegion is the region a SigV4 signature for endpoint must be scoped to —
// the region of the Function URL itself, read from its hostname.
//
// It used to be the client's ambient region (cfg.Region), falling back to
// us-east-1 when unset. That was right only by accident: LoadDefaultConfig does
// not read the region from IMDS, so on an instance cfg.Region is usually empty and
// the fallback happened to match the URL. Any spored environment that set
// AWS_REGION outside us-east-1 would have signed for the wrong region and been
// refused. The endpoint's own hostname cannot disagree with the endpoint.
//
// fallback (the ambient region, else us-east-1) is used only for a custom
// endpoint whose hostname carries no region.
func signingRegion(endpoint, fallback string) string {
	if u, err := url.Parse(endpoint); err == nil {
		if m := functionURLHost.FindStringSubmatch(strings.ToLower(u.Hostname())); m != nil {
			return m[1]
		}
	}
	if fallback != "" {
		return fallback
	}
	return "us-east-1"
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
		Lease:                     c.renewing,
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
		Lease:                     c.renewing,
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

	// Check the HTTP status BEFORE trying to parse the body as our JSON schema.
	// A Function URL running under AuthType: AWS_IAM rejects an unsigned or
	// bad-signature request with a 403 whose body is AWS's own JSON
	// (`{"Message":"Forbidden"}`) — which unmarshals cleanly into an all-empty
	// DNSUpdateResponse, turning a hard auth failure into a useless
	// `DNS API error: ` (empty). Surface the status and a body snippet so the
	// real cause (e.g. SigV4 rejection post-#173 cutover) is visible instead of
	// silently swallowed (#435).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(respBody))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return nil, fmt.Errorf("DNS API returned HTTP %d: %s", resp.StatusCode, snippet)
	}

	// Parse response
	var dnsResp DNSUpdateResponse
	if err := json.Unmarshal(respBody, &dnsResp); err != nil {
		return nil, fmt.Errorf("failed to parse response (HTTP %d): %w", resp.StatusCode, err)
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
	// Fail closed on empty credentials. Some credential-chain states return a
	// blank (anonymous) credential WITHOUT an error — e.g. IMDS not yet reachable
	// at agent startup. Signing with an empty AccessKeyID still "succeeds" locally
	// but yields a signature the AWS_IAM Function URL rejects with a bare 403 and
	// no invocation — indistinguishable from "not signed at all", which is exactly
	// the silent-DNS-failure symptom (#435). Surface it as a clear error instead.
	if creds.AccessKeyID == "" {
		return fmt.Errorf("DNS request signing enabled (SPORE_DNS_SIGV4) but resolved AWS credentials are empty — the instance role may not be reachable via IMDS yet; the AWS_IAM DNS Function URL will reject an unsigned request")
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
