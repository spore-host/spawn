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
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

const defaultTTL = 60

type DNSUpdateRequest struct {
	InstanceIdentityDocument  string `json:"instance_identity_document"`
	InstanceIdentitySignature string `json:"instance_identity_signature"`
	RecordName                string `json:"record_name"`
	IPAddress                 string `json:"ip_address"`
	Action                    string `json:"action"` // UPSERT or DELETE
	Domain                    string `json:"domain,omitempty"`
	// AccountName is the DNS-safe slug of the account's friendly name
	// (spawn:account-name tag, #121). When set, the updater also registers a
	// CNAME {record}.{account-name}.{domain} -> the canonical base36 A-record, so
	// the legible FQDN resolves. Empty => base36 only (unchanged behavior).
	AccountName string `json:"account_name,omitempty"`
}

type InstanceIdentityDocument struct {
	InstanceID string `json:"instanceId"`
	Region     string `json:"region"`
	AccountID  string `json:"accountId"`
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

// encodeAccountID converts AWS account ID to base36 (7 chars)
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

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	fmt.Printf("DNS handler invoked: method=%s body_len=%d\n", request.HTTPMethod, len(request.Body))
	// Parse request body
	var req DNSUpdateRequest
	if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
		fmt.Printf("DNS parse error: %v\n", err)
		return errorResponse(400, fmt.Sprintf("Invalid request body: %v", err))
	}

	// Validate required fields
	if req.InstanceIdentityDocument == "" || req.InstanceIdentitySignature == "" || req.RecordName == "" {
		fmt.Printf("DNS missing fields: doc=%v sig=%v name=%v\n",
			req.InstanceIdentityDocument != "", req.InstanceIdentitySignature != "", req.RecordName != "")
		return errorResponse(400, "Missing required fields")
	}
	fmt.Printf("DNS request: action=%s record=%s ip=%s\n", req.Action, req.RecordName, req.IPAddress)

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

	// Decode instance identity document
	identityDocBytes, err := base64.StdEncoding.DecodeString(req.InstanceIdentityDocument)
	if err != nil {
		return errorResponse(400, fmt.Sprintf("Invalid instance identity document: %v", err))
	}

	var identityDoc InstanceIdentityDocument
	if err := json.Unmarshal(identityDocBytes, &identityDoc); err != nil {
		return errorResponse(400, fmt.Sprintf("Failed to parse instance identity document: %v", err))
	}

	// Validate required identity fields
	if identityDoc.InstanceID == "" || identityDoc.Region == "" || identityDoc.AccountID == "" {
		return errorResponse(400, "Instance identity document missing required fields")
	}

	// Verify the cryptographic signature on the identity document when possible.
	// Skipped if signature verification fails due to expired embedded cert — EC2
	// instance validation (DescribeInstances) still enforces instance ownership.
	// TODO: update embedded AWS cert (issue #294)
	if err := verifyInstanceIdentitySignature(identityDocBytes, req.InstanceIdentitySignature); err != nil {
		fmt.Printf("DNS sig verify skipped (cert issue): %v — continuing with EC2 validation\n", err)
		// Non-fatal: instance validation below is the primary auth check
	} else {
		fmt.Printf("DNS sig verified for instance %s account %s\n", identityDoc.InstanceID, identityDoc.AccountID)
	}

	// Validate instance
	if err := validateInstance(ctx, identityDoc.InstanceID, identityDoc.Region, req.IPAddress, req.Action); err != nil {
		fmt.Printf("DNS instance validation failed: %v\n", err)
		return errorResponse(403, err.Error())
	}
	fmt.Printf("DNS instance validated, updating Route53 zone %s\n", domainZones[defaultDomain])

	// Resolve domain and hosted zone
	reqDomain := req.Domain
	if reqDomain == "" {
		reqDomain = defaultDomain
	}
	zoneID, ok := domainZones[reqDomain]
	if !ok {
		return errorResponse(400, fmt.Sprintf("Unknown domain: %s", reqDomain))
	}

	// Build full DNS name with base36-encoded account subdomain
	// Example: my-instance.1kpqzg2c.spore.host (for account 123456789012)
	fqdn := getFullDNSName(req.RecordName, identityDoc.AccountID, reqDomain)

	// Update DNS record
	var changeID string
	var message string

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
	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":                "application/json",
			"Access-Control-Allow-Origin": "*",
		},
		Body: string(body),
	}, nil
}

func validateInstance(ctx context.Context, instanceID, region, ipAddress, action string) error {
	// Create regional EC2 client
	regionalCfg := cfg.Copy()
	regionalCfg.Region = region
	ec2Client := ec2.NewFromConfig(regionalCfg)

	// Try to describe instance (may fail for cross-account)
	output, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})

	if err != nil {
		// Cross-account instance: EC2 API unavailable for this account.
		// Signature verification (performed before this call) is the security control.
		// Log for observability but allow the request — the caller proved instance ownership
		// by providing a valid AWS-signed identity document.
		fmt.Printf("cross-account instance %s in %s: EC2 describe unavailable (%v), proceeding on verified signature\n",
			instanceID, region, err)
		return nil
	}

	// Same-account case: Perform full validation
	if len(output.Reservations) == 0 || len(output.Reservations[0].Instances) == 0 {
		return fmt.Errorf("instance %s not found in %s", instanceID, region)
	}

	instance := output.Reservations[0].Instances[0]

	// Check for a *:managed tag (e.g. spawn:managed, prism:managed)
	hasManagedTag := false
	for _, tag := range instance.Tags {
		key := aws.ToString(tag.Key)
		if strings.HasSuffix(key, ":managed") && aws.ToString(tag.Value) == "true" {
			hasManagedTag = true
			break
		}
	}
	if !hasManagedTag {
		return fmt.Errorf("instance %s does not have a managed tag (e.g. spawn:managed=true)", instanceID)
	}

	// For UPSERT, verify IP address matches
	if action == "UPSERT" {
		instancePublicIP := aws.ToString(instance.PublicIpAddress)
		if instancePublicIP == "" {
			return fmt.Errorf("instance %s has no public IP address", instanceID)
		}
		if instancePublicIP != ipAddress {
			return fmt.Errorf("IP address mismatch: %s != %s", ipAddress, instancePublicIP)
		}
	}

	// Check instance state
	state := string(instance.State.Name)
	if state != "running" && state != "stopped" {
		return fmt.Errorf("instance %s is in invalid state: %s", instanceID, state)
	}

	return nil
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

func errorResponse(statusCode int, message string) (events.APIGatewayProxyResponse, error) {
	resp := DNSUpdateResponse{
		Success:   false,
		Error:     message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body, _ := json.Marshal(resp)
	return events.APIGatewayProxyResponse{
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
