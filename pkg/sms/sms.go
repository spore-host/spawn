// Package sms provides shared SMS utilities for spore.host lifecycle notifications.
// Used by both the spore-bot Lambda (outbound) and the rest-api Lambda (inbound replies).
package sms

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

const PendingTable = "spore-sms-pending"

// PendingNotification tracks a sent SMS while waiting for the user's numbered reply.
// Keyed by twilioNumber#userPhone so each project's number scopes its own reply state.
type PendingNotification struct {
	TwilioNumber string // the Twilio number that sent the message (identifies the project)
	UserPhone    string // the recipient's phone number
	Project      string // "spore", "prism", etc.
	InstanceID   string
	Region       string
	EventType    string
	Options      map[string]string // "1" -> "extend:1h", "0" -> "dismiss", etc.
}

// PendingKey returns the DynamoDB primary key for a (twilioNumber, userPhone) pair.
func PendingKey(twilioNumber, userPhone string) string {
	return twilioNumber + "#" + userPhone
}

// ProjectNumber returns the Twilio phone number for a project.
// Reads TWILIO_NUMBER_{PROJECT} from the environment (e.g. TWILIO_NUMBER_SPORE).
func ProjectNumber(project string) string {
	return os.Getenv("TWILIO_NUMBER_" + strings.ToUpper(project))
}

// Send sends an outbound SMS from the project's dedicated Twilio number.
func Send(ctx context.Context, project, toPhone, message string) error {
	accountSID := os.Getenv("TWILIO_ACCOUNT_SID")
	authToken := os.Getenv("TWILIO_AUTH_TOKEN")
	fromNumber := ProjectNumber(project)

	if accountSID == "" || authToken == "" || fromNumber == "" {
		return fmt.Errorf("Twilio not configured for project %q", project)
	}

	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", accountSID)
	body := url.Values{
		"To":   {toPhone},
		"From": {fromNumber},
		"Body": {message},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(body.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(accountSID, authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("twilio request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("twilio %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// StorePending saves a pending notification keyed by (twilioNumber, userPhone).
// The entry expires after 15 minutes via DynamoDB TTL.
func StorePending(ctx context.Context, n PendingNotification) error {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return err
	}
	expires := time.Now().Add(15 * time.Minute).Unix()
	_, err = dynamodb.NewFromConfig(cfg).PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(PendingTable),
		Item: map[string]dynamodbtypes.AttributeValue{
			"pending_key": &dynamodbtypes.AttributeValueMemberS{Value: PendingKey(n.TwilioNumber, n.UserPhone)},
			"project":     &dynamodbtypes.AttributeValueMemberS{Value: n.Project},
			"instance_id": &dynamodbtypes.AttributeValueMemberS{Value: n.InstanceID},
			"region":      &dynamodbtypes.AttributeValueMemberS{Value: n.Region},
			"event_type":  &dynamodbtypes.AttributeValueMemberS{Value: n.EventType},
			"options":     &dynamodbtypes.AttributeValueMemberS{Value: encodeOptions(n.Options)},
			"ttl":         &dynamodbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", expires)},
		},
	})
	return err
}

// BuildMessage returns the SMS body and reply options for a lifecycle event.
func BuildMessage(instanceName, eventType string, extra map[string]string) (string, map[string]string) {
	options := map[string]string{}
	var b strings.Builder

	switch eventType {
	case "ttl_warning":
		fmt.Fprintf(&b, "%s terminates in %s.\n\n1 · Extend 1h\n2 · Extend 2h\n4 · Extend 4h\n0 · Dismiss",
			instanceName, extra["remaining"])
		options = map[string]string{"1": "extend:1h", "2": "extend:2h", "4": "extend:4h", "0": "dismiss"}

	case "idle_warning":
		fmt.Fprintf(&b, "%s idle %s, stops in %s.\n\n1 · Keep running\n0 · Dismiss",
			instanceName, extra["idle_duration"], extra["remaining"])
		options = map[string]string{"1": "keep", "0": "dismiss"}

	case "idle_stopped":
		fmt.Fprintf(&b, "%s stopped (idle). Cost so far: %s\n\n1 · Wake instance\n0 · Dismiss",
			instanceName, extra["cost"])
		options = map[string]string{"1": "start", "0": "dismiss"}

	case "idle_hibernated":
		fmt.Fprintf(&b, "%s hibernated (idle). Cost so far: %s\n\n1 · Wake instance\n0 · Dismiss",
			instanceName, extra["cost"])
		options = map[string]string{"1": "start", "0": "dismiss"}

	case "ttl_expired":
		fmt.Fprintf(&b, "%s terminated (TTL). Cumulative cost: %s", instanceName, extra["cost"])

	case "completion":
		fmt.Fprintf(&b, "%s job done. Cost: %s\n\n1 · Get status\n0 · Dismiss",
			instanceName, extra["cost"])
		options = map[string]string{"1": "status", "0": "dismiss"}

	case "spot_interrupt":
		fmt.Fprintf(&b, "%s Spot interruption — ~2 min remaining.", instanceName)

	case "pre_stop_start":
		fmt.Fprintf(&b, "%s is running its shutdown task before stopping.", instanceName)

	case "pre_stop_failed":
		fmt.Fprintf(&b, "⚠️ %s shutdown task FAILED — output may not have been saved. %s", instanceName, extra["detail"])

	case "pre_stop_timeout":
		fmt.Fprintf(&b, "⚠️ %s shutdown task TIMED OUT — output may be incomplete. %s", instanceName, extra["detail"])

	default:
		fmt.Fprintf(&b, "%s: %s", instanceName, eventType)
	}

	return b.String(), options
}

func encodeOptions(opts map[string]string) string {
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
