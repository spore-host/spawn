package main

// SMS notification helpers for spore-bot.
// Mirrors spawn/pkg/sms but kept local to avoid pulling spawn's full cmd/ tree
// into this Lambda's module graph.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const smsPendingTable = "spore-sms-pending"

// twilioProjectNumber returns the Twilio phone number for a project from env vars.
func twilioProjectNumber(project string) string {
	return os.Getenv("TWILIO_NUMBER_" + strings.ToUpper(project))
}

// twilioSend sends an outbound SMS via the Twilio API.
func twilioSend(ctx context.Context, project, toPhone, message string) error {
	accountSID := os.Getenv("TWILIO_ACCOUNT_SID")
	authToken := os.Getenv("TWILIO_AUTH_TOKEN")
	fromNumber := twilioProjectNumber(project)
	if accountSID == "" || authToken == "" || fromNumber == "" {
		return fmt.Errorf("Twilio not configured for project %q", project)
	}
	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", accountSID)
	body := url.Values{"To": {toPhone}, "From": {fromNumber}, "Body": {message}}
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(body.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(accountSID, authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("twilio %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// twilioStorePending saves a pending notification to DynamoDB so replies can be matched.
func twilioStorePending(ctx context.Context, fromNumber, toPhone, project, instanceID, region, eventType string, options map[string]string) error {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return err
	}
	expires := time.Now().Add(15 * time.Minute).Unix()
	key := fromNumber + "#" + toPhone
	opts := encodeOpts(options)
	_, err = dynamodb.NewFromConfig(cfg).PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(smsPendingTable),
		Item: map[string]dynamodbtypes.AttributeValue{
			"pending_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
			"project":     &dynamodbtypes.AttributeValueMemberS{Value: project},
			"instance_id": &dynamodbtypes.AttributeValueMemberS{Value: instanceID},
			"region":      &dynamodbtypes.AttributeValueMemberS{Value: region},
			"event_type":  &dynamodbtypes.AttributeValueMemberS{Value: eventType},
			"options":     &dynamodbtypes.AttributeValueMemberS{Value: opts},
			"ttl":         &dynamodbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", expires)},
		},
	})
	return err
}

// twilioMessage builds the SMS text and reply options for a lifecycle event.
func twilioMessage(instanceName, eventType string, extra map[string]string) (string, map[string]string) {
	get := func(k, def string) string {
		if v := extra[k]; v != "" {
			return v
		}
		return def
	}
	options := map[string]string{}
	var b strings.Builder
	switch eventType {
	case "ttl_warning":
		fmt.Fprintf(&b, "%s terminates in %s.\n\n1 · Extend 1h\n2 · Extend 2h\n4 · Extend 4h\n0 · Dismiss",
			instanceName, get("remaining", "5 minutes"))
		options = map[string]string{"1": "extend:1h", "2": "extend:2h", "4": "extend:4h", "0": "dismiss"}
	case "idle_warning":
		fmt.Fprintf(&b, "%s idle %s, stops in %s.\n\n1 · Keep running\n0 · Dismiss",
			instanceName, get("idle_duration", "25m"), get("remaining", "5 minutes"))
		options = map[string]string{"1": "keep", "0": "dismiss"}
	case "idle_stopped":
		fmt.Fprintf(&b, "%s stopped (idle). Cost so far: %s\n\n1 · Wake instance\n0 · Dismiss",
			instanceName, get("cost", "$0.00"))
		options = map[string]string{"1": "start", "0": "dismiss"}
	case "idle_hibernated":
		fmt.Fprintf(&b, "%s hibernated (idle). Cost so far: %s\n\n1 · Wake instance\n0 · Dismiss",
			instanceName, get("cost", "$0.00"))
		options = map[string]string{"1": "start", "0": "dismiss"}
	case "ttl_expired":
		fmt.Fprintf(&b, "%s terminated (TTL). Cumulative cost: %s", instanceName, get("cost", "$0.00"))
	case "completion":
		fmt.Fprintf(&b, "%s job done. Cost: %s\n\n1 · Get status\n0 · Dismiss",
			instanceName, get("cost", "$0.00"))
		options = map[string]string{"1": "status", "0": "dismiss"}
	case "spot_interrupt":
		fmt.Fprintf(&b, "%s Spot interruption — ~2 min remaining.", instanceName)
	case "pre_stop_start":
		fmt.Fprintf(&b, "%s is running its shutdown task before stopping.", instanceName)
	default:
		fmt.Fprintf(&b, "%s: %s", instanceName, eventType)
	}
	return b.String(), options
}

func encodeOpts(opts map[string]string) string {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+opts[k])
	}
	return strings.Join(parts, ",")
}
