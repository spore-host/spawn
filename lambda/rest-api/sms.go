package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/sms"
)

// handleSMSIncoming processes a Twilio webhook for an inbound SMS reply.
// The To field identifies the project; the From field identifies the user.
func handleSMSIncoming(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	authToken := os.Getenv("TWILIO_AUTH_TOKEN")
	skipSig := os.Getenv("SKIP_TWILIO_SIGNATURE") == "true"
	if authToken != "" && !skipSig && !validateTwilioSignature(req, authToken) {
		return errResp(http.StatusForbidden, "invalid Twilio signature"), nil
	}

	rawBody := req.Body
	if req.IsBase64Encoded {
		if decoded, err := base64.StdEncoding.DecodeString(rawBody); err == nil {
			rawBody = string(decoded)
		}
	}
	params, err := url.ParseQuery(rawBody)
	if err != nil {
		return errResp(http.StatusBadRequest, "invalid body"), nil
	}

	to := params.Get("To")
	from := params.Get("From")
	body := strings.TrimSpace(params.Get("Body"))

	if to == "" || from == "" || body == "" {
		return twilioResp("")
	}

	pending, err := fetchPending(ctx, sms.PendingKey(to, from))
	if err != nil || pending == nil {
		return twilioResp("No pending notification. Use the spore.host CLI or Slack bot to manage your instances.")
	}

	action, ok := pending.Options[body]
	if !ok {
		return twilioResp(fmt.Sprintf("Reply %q not recognised. Valid: %s", body, buildOptionsHint(pending.Options)))
	}

	if action == "dismiss" {
		clearPending(ctx, sms.PendingKey(to, from))
		return twilioResp("Dismissed.")
	}

	reply, err := executeAction(ctx, pending, action)
	if err != nil {
		return twilioResp(fmt.Sprintf("Error: %v", err))
	}

	clearPending(ctx, sms.PendingKey(to, from))
	return twilioResp(reply)
}

func executeAction(ctx context.Context, p *sms.PendingNotification, action string) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(p.Region))
	if err != nil {
		return "", fmt.Errorf("AWS config: %w", err)
	}
	client := spawnclient.NewClientFromConfig(cfg)

	switch {
	case action == "start":
		if err := client.StartInstance(ctx, p.Region, p.InstanceID); err != nil {
			return "", err
		}
		return "Waking instance. Use `spawn connect` to reconnect when running.", nil

	case action == "status":
		state, err := client.GetInstanceState(ctx, p.Region, p.InstanceID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Instance state: %s", state), nil

	case strings.HasPrefix(action, "extend:"):
		dur := strings.TrimPrefix(action, "extend:")
		if err := client.UpdateInstanceTags(ctx, p.Region, p.InstanceID, map[string]string{
			"spawn:ttl": dur,
		}); err != nil {
			return "", err
		}
		return fmt.Sprintf("TTL extended by %s.", dur), nil

	case action == "keep":
		if err := client.UpdateInstanceTags(ctx, p.Region, p.InstanceID, map[string]string{
			"spawn:idle-reset": time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return "", err
		}
		return "Idle timer reset. Instance will keep running.", nil

	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
}

// handleNotificationRegister saves or removes a phone number for a user.
func handleNotificationRegister(ctx context.Context, method string, req events.APIGatewayV2HTTPRequest, p *Principal) (events.APIGatewayV2HTTPResponse, error) {
	var body struct {
		Phone   string `json:"phone"`
		UserKey string `json:"user_key"`
	}
	if err := parseJSON(req.Body, &body); err != nil || body.Phone == "" || body.UserKey == "" {
		return errResp(http.StatusBadRequest, "phone and user_key required"), nil
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return errResp(http.StatusInternalServerError, "AWS config error"), nil
	}
	table := os.Getenv("REGISTRY_TABLE")
	if table == "" {
		table = "spore-bot-registry"
	}
	client := dynamodb.NewFromConfig(cfg)

	if method == "DELETE" {
		_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(table),
			Key: map[string]dynamodbtypes.AttributeValue{
				"user_key": &dynamodbtypes.AttributeValueMemberS{Value: body.UserKey},
				"nickname": &dynamodbtypes.AttributeValueMemberS{Value: "_phone"},
			},
			UpdateExpression: aws.String("REMOVE phone"),
		})
	} else {
		_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(table),
			Item: map[string]dynamodbtypes.AttributeValue{
				"user_key": &dynamodbtypes.AttributeValueMemberS{Value: body.UserKey},
				"nickname": &dynamodbtypes.AttributeValueMemberS{Value: "_phone"},
				"phone":    &dynamodbtypes.AttributeValueMemberS{Value: body.Phone},
				"project":  &dynamodbtypes.AttributeValueMemberS{Value: p.Project},
			},
		})
	}
	if err != nil {
		return errResp(http.StatusInternalServerError, fmt.Sprintf("DynamoDB: %v", err)), nil
	}
	verb := "registered"
	if method == "DELETE" {
		verb = "deregistered"
	}
	return jsonResp(http.StatusOK, map[string]string{"status": verb, "phone": body.Phone}), nil
}

func twilioResp(msg string) (events.APIGatewayV2HTTPResponse, error) {
	var body string
	if msg == "" {
		body = `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`
	} else {
		body = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><Response><Message>%s</Message></Response>`, msg)
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/xml"},
		Body:       body,
	}, nil
}

func validateTwilioSignature(req events.APIGatewayV2HTTPRequest, authToken string) bool {
	urlStr := "https://" + req.RequestContext.DomainName + req.RequestContext.HTTP.Path
	params, _ := url.ParseQuery(req.Body)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(params.Get(k))
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(urlStr + sb.String()))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(req.Headers["x-twilio-signature"]))
}

func fetchPending(ctx context.Context, key string) (*sms.PendingNotification, error) {
	cfg, _ := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	out, err := dynamodb.NewFromConfig(cfg).GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(sms.PendingTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"pending_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
		},
	})
	if err != nil || out.Item == nil {
		return nil, nil
	}
	get := func(k string) string {
		if v, ok := out.Item[k].(*dynamodbtypes.AttributeValueMemberS); ok {
			return v.Value
		}
		return ""
	}
	parts := strings.SplitN(key, "#", 2)
	twilioNum, userPhone := "", key
	if len(parts) == 2 {
		twilioNum, userPhone = parts[0], parts[1]
	}
	return &sms.PendingNotification{
		TwilioNumber: twilioNum,
		UserPhone:    userPhone,
		Project:      get("project"),
		InstanceID:   get("instance_id"),
		Region:       get("region"),
		EventType:    get("event_type"),
		Options:      decodeOptions(get("options")),
	}, nil
}

func clearPending(ctx context.Context, key string) {
	cfg, _ := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	_, _ = dynamodb.NewFromConfig(cfg).DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(sms.PendingTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"pending_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
		},
	})
}

func decodeOptions(s string) map[string]string {
	m := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			m[kv[0]] = kv[1]
		}
	}
	return m
}

func buildOptionsHint(opts map[string]string) string {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
