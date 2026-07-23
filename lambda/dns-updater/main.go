package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

const defaultTTL = 60

type DNSUpdateRequest struct {
	RecordName string `json:"record_name"`
	IPAddress  string `json:"ip_address"`
	Action     string `json:"action"` // UPSERT or DELETE
	Domain     string `json:"domain,omitempty"`
	// AccountName is the DNS-safe slug of the account's friendly name
	// (spawn:account-name tag, #121). When set, the updater also registers a
	// CNAME {record}.{account-name}.{domain} -> the canonical base36 A-record, so
	// the legible FQDN resolves. Empty => base36 only (unchanged behavior).
	AccountName string `json:"account_name,omitempty"`
	// Note (#173): instance_identity_document / _signature were removed — the
	// caller is now authenticated by SigV4 (AuthType: AWS_IAM) and the record is
	// namespaced under the verified caller account, not anything in the body.
	// Older clients may still send those keys; unknown JSON fields are ignored.
}

type DNSUpdateResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
	Record    string `json:"record,omitempty"`
	ChangeID  string `json:"change_id,omitempty"`
	Timestamp string `json:"timestamp"`
}

var (
	cfg           aws.Config
	route53Client *route53.Client

	// domainZones maps domain names to Route53 hosted zone IDs.
	// Parsed from the DOMAIN_ZONES env var: "spore.host=Z048...,prismcloud.host=Z09ABC..."
	domainZones   map[string]string
	defaultDomain string
)

// encodeAccountID converts AWS account ID to base36 (≤8 chars)
func encodeAccountID(accountID string) string {
	n := new(big.Int)
	n.SetString(accountID, 10)
	return strings.ToLower(n.Text(36))
}

// getFullDNSName returns the complete DNS name with base36-encoded account subdomain.
// Example: ("my-instance", "123456789012", "spore.host") -> "my-instance.1kpqzg2c.spore.host"
func getFullDNSName(recordName, accountID, dom string) string {
	encoded := encodeAccountID(accountID)
	return fmt.Sprintf("%s.%s.%s", recordName, encoded, dom)
}

// dnsLabelRe matches a valid RFC-1035 DNS label (the form spawn's
// slugifyDNSLabel produces). The Lambda re-validates the caller-supplied
// account-name slug before splicing it into a Route53 record name — never trust
// it blindly even though it's signed-instance traffic.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// aliasDNSName returns the friendly FQDN {record}.{account-name}.{domain}, or
// "" if the account-name slug isn't a valid DNS label.
func aliasDNSName(recordName, accountName, dom string) string {
	if !dnsLabelRe.MatchString(accountName) {
		return ""
	}
	return fmt.Sprintf("%s.%s.%s", recordName, accountName, dom)
}

func init() {
	var err error
	cfg, err = config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(fmt.Sprintf("unable to load SDK config: %v", err))
	}
	route53Client = route53.NewFromConfig(cfg)

	// Parse DOMAIN_ZONES env var: "spore.host=Z048...,prismcloud.host=Z09ABC..."
	domainZones = make(map[string]string)
	if zones := os.Getenv("DOMAIN_ZONES"); zones != "" {
		for _, entry := range strings.Split(zones, ",") {
			parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
			if len(parts) == 2 {
				domainZones[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	// Backward compatibility: if DOMAIN_ZONES is empty, use legacy env vars or defaults
	if len(domainZones) == 0 {
		zoneID := os.Getenv("HOSTED_ZONE_ID")
		if zoneID == "" {
			zoneID = "Z048907324UNXKEK9KX93" // legacy default
		}
		domainZones["spore.host"] = zoneID
	}

	defaultDomain = os.Getenv("DEFAULT_DOMAIN")
	if defaultDomain == "" {
		defaultDomain = "spore.host"
	}
}

func handler(ctx context.Context, request events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	fmt.Printf("DNS handler invoked: method=%s body_len=%d\n", request.RequestContext.HTTP.Method, len(request.Body))

	// Decode the body (Function URLs may base64-encode it).
	rawBody := request.Body
	if request.IsBase64Encoded {
		if decoded, derr := base64.StdEncoding.DecodeString(rawBody); derr == nil {
			rawBody = string(decoded)
		}
	}

	// Parse request body
	var req DNSUpdateRequest
	if err := json.Unmarshal([]byte(rawBody), &req); err != nil {
		fmt.Printf("DNS parse error: %v\n", err)
		return errorResponse(400, fmt.Sprintf("Invalid request body: %v", err))
	}

	if req.RecordName == "" {
		return errorResponse(400, "Missing required field: record_name")
	}

	// Validate action
	req.Action = strings.ToUpper(req.Action)
	if req.Action == "" {
		req.Action = "UPSERT"
	}
	if req.Action != "UPSERT" && req.Action != "DELETE" {
		return errorResponse(400, "Invalid action (must be UPSERT or DELETE)")
	}

	// Validate IP address for UPSERT
	if req.Action == "UPSERT" && req.IPAddress == "" {
		return errorResponse(400, "IP address required for UPSERT")
	}

	// Validate record name format
	req.RecordName = strings.ToLower(strings.TrimSpace(req.RecordName))
	validName := regexp.MustCompile(`^[a-z0-9-]+$`)
	if !validName.MatchString(req.RecordName) {
		return errorResponse(400, "Invalid record name (alphanumeric and hyphens only)")
	}

	// ── Authorize via the SigV4-verified caller account (#173) ──────────────────
	// The Function URL runs under AuthType: AWS_IAM, so every request that reaches
	// this handler has already passed SigV4 verification and carries the verified
	// caller account in requestContext.authorizer.iam. The record is namespaced
	// under THAT account — unspoofable, and independent of anything in the request
	// body. No identity document, no signature, no embedded cert, no per-region
	// maintenance (the reason IAM auth was chosen over PKCS#7; see #294). A request
	// without the authorizer can't occur under AWS_IAM, but we reject it defensively.
	authz := request.RequestContext.Authorizer
	if authz == nil || authz.IAM == nil || authz.IAM.AccountID == "" {
		fmt.Printf("DNS request without an IAM authorizer — rejecting (Function URL must be AuthType: AWS_IAM)\n")
		return errorResponse(403, "missing IAM authorizer")
	}
	accountID := authz.IAM.AccountID
	fmt.Printf("DNS authorized via AWS_IAM: verified account %s (caller %s)\n", accountID, authz.IAM.UserARN)

	// Resolve domain and hosted zone
	reqDomain := req.Domain
	if reqDomain == "" {
		reqDomain = defaultDomain
	}
	zoneID, ok := domainZones[reqDomain]
	if !ok {
		return errorResponse(400, fmt.Sprintf("Unknown domain: %s", reqDomain))
	}

	// Build full DNS name with base36-encoded account subdomain, anchored to the
	// authorized account — a caller can only write records under its own account's
	// subdomain. Example: my-instance.1kpqzg2c.spore.host (for account 123456789012)
	fqdn := getFullDNSName(req.RecordName, accountID, reqDomain)

	// Update DNS record
	var changeID string
	var message string
	var err error

	// Optional friendly alias: {record}.{account-name}.{domain} as a CNAME to the
	// canonical base36 A-record (#121). base36 stays authoritative (holds the IP);
	// the alias just points at it, so the IP is updated in one place.
	alias := aliasDNSName(req.RecordName, req.AccountName, reqDomain)

	if req.Action == "UPSERT" {
		changeID, err = upsertDNSRecord(ctx, fqdn, req.IPAddress, zoneID)
		if err != nil {
			return errorResponse(500, fmt.Sprintf("Failed to update DNS: %v", err))
		}
		message = fmt.Sprintf("DNS record updated: %s -> %s", fqdn, req.IPAddress)
		if alias != "" {
			if _, aerr := upsertCNAMERecord(ctx, alias, fqdn, zoneID); aerr != nil {
				// Non-fatal: the canonical A-record already succeeded. Log and go on.
				fmt.Printf("warning: failed to upsert alias CNAME %s -> %s: %v\n", alias, fqdn, aerr)
			} else {
				message += fmt.Sprintf(" (alias %s)", alias)
			}
		}
	} else {
		changeID, err = deleteDNSRecord(ctx, fqdn, zoneID)
		if err != nil {
			return errorResponse(500, fmt.Sprintf("Failed to delete DNS: %v", err))
		}
		message = fmt.Sprintf("DNS record deleted: %s", fqdn)
		if alias != "" {
			if _, aerr := deleteCNAMERecord(ctx, alias, zoneID); aerr != nil {
				fmt.Printf("warning: failed to delete alias CNAME %s: %v\n", alias, aerr)
			}
		}
	}

	// Success response
	resp := DNSUpdateResponse{
		Success:   true,
		Message:   message,
		Record:    fqdn,
		ChangeID:  changeID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body, _ := json.Marshal(resp)
	return events.LambdaFunctionURLResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":                "application/json",
			"Access-Control-Allow-Origin": "*",
		},
		Body: string(body),
	}, nil
}

func upsertDNSRecord(ctx context.Context, fqdn, ipAddress, zoneID string) (string, error) {
	comment := fmt.Sprintf("Updated by spawn instance at %s", time.Now().UTC().Format(time.RFC3339))

	output, err := route53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String(comment),
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: aws.String(fqdn),
						Type: types.RRTypeA,
						TTL:  aws.Int64(defaultTTL),
						ResourceRecords: []types.ResourceRecord{
							{Value: aws.String(ipAddress)},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return "", err
	}

	return aws.ToString(output.ChangeInfo.Id), nil
}

func deleteDNSRecord(ctx context.Context, fqdn, zoneID string) (string, error) {
	// First, get the current record
	listOutput, err := route53Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(zoneID),
		StartRecordName: aws.String(fqdn),
		StartRecordType: types.RRTypeA,
		MaxItems:        aws.Int32(1),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list records: %w", err)
	}

	// Find matching record
	var recordToDelete *types.ResourceRecordSet
	for _, recordSet := range listOutput.ResourceRecordSets {
		if strings.TrimSuffix(aws.ToString(recordSet.Name), ".") == fqdn && recordSet.Type == types.RRTypeA {
			recordToDelete = &recordSet
			break
		}
	}

	if recordToDelete == nil {
		return "", fmt.Errorf("DNS record %s not found", fqdn)
	}

	// Delete the record
	comment := fmt.Sprintf("Deleted by spawn instance at %s", time.Now().UTC().Format(time.RFC3339))
	output, err := route53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String(comment),
			Changes: []types.Change{
				{
					Action:            types.ChangeActionDelete,
					ResourceRecordSet: recordToDelete,
				},
			},
		},
	})
	if err != nil {
		return "", err
	}

	return aws.ToString(output.ChangeInfo.Id), nil
}

// upsertCNAMERecord points aliasFQDN at targetFQDN (the canonical base36 record)
// via a CNAME, so the friendly account-name FQDN resolves to the same instance
// without duplicating the IP (#121).
func upsertCNAMERecord(ctx context.Context, aliasFQDN, targetFQDN, zoneID string) (string, error) {
	comment := fmt.Sprintf("Alias upserted by spawn instance at %s", time.Now().UTC().Format(time.RFC3339))
	output, err := route53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String(comment),
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: aws.String(aliasFQDN),
						Type: types.RRTypeCname,
						TTL:  aws.Int64(defaultTTL),
						ResourceRecords: []types.ResourceRecord{
							{Value: aws.String(targetFQDN)},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(output.ChangeInfo.Id), nil
}

// deleteCNAMERecord removes the alias CNAME. Best-effort: a missing record is
// not an error (the canonical A-record delete is what matters).
func deleteCNAMERecord(ctx context.Context, aliasFQDN, zoneID string) (string, error) {
	listOutput, err := route53Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(zoneID),
		StartRecordName: aws.String(aliasFQDN),
		StartRecordType: types.RRTypeCname,
		MaxItems:        aws.Int32(1),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list alias records: %w", err)
	}

	var recordToDelete *types.ResourceRecordSet
	for i := range listOutput.ResourceRecordSets {
		rs := listOutput.ResourceRecordSets[i]
		if strings.TrimSuffix(aws.ToString(rs.Name), ".") == aliasFQDN && rs.Type == types.RRTypeCname {
			recordToDelete = &rs
			break
		}
	}
	if recordToDelete == nil {
		return "", nil // already gone — fine
	}

	comment := fmt.Sprintf("Alias deleted by spawn instance at %s", time.Now().UTC().Format(time.RFC3339))
	output, err := route53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String(comment),
			Changes: []types.Change{
				{Action: types.ChangeActionDelete, ResourceRecordSet: recordToDelete},
			},
		},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(output.ChangeInfo.Id), nil
}

func errorResponse(statusCode int, message string) (events.LambdaFunctionURLResponse, error) {
	resp := DNSUpdateResponse{
		Success:   false,
		Error:     message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body, _ := json.Marshal(resp)
	return events.LambdaFunctionURLResponse{
		StatusCode: statusCode,
		Headers: map[string]string{
			"Content-Type":                "application/json",
			"Access-Control-Allow-Origin": "*",
		},
		Body: string(body),
	}, nil
}

func main() {
	lambda.Start(handler)
}
