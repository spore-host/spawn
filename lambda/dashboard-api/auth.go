package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// getUserFromRequest extracts user identity from API Gateway request
// Returns: userID, cliIamArn, accountBase36, error
func getUserFromRequest(ctx context.Context, cfg aws.Config, request events.APIGatewayProxyRequest) (string, string, string, error) {
	// Check for credentials in X-AWS-Credentials header (from Cognito Identity Pool)
	if credsHeader, ok := request.Headers["x-aws-credentials"]; ok && credsHeader != "" {
		return getUserFromCredentialsHeader(ctx, cfg, request, credsHeader)
	}

	// Try case-insensitive header lookup (API Gateway may normalize headers)
	for key, value := range request.Headers {
		if strings.ToLower(key) == "x-aws-credentials" && value != "" {
			return getUserFromCredentialsHeader(ctx, cfg, request, value)
		}
	}

	// Fallback: IAM authentication via API Gateway
	// Extract user ARN from request context
	var userID string

	// Try to get user ARN from Identity (IAM auth)
	if request.RequestContext.Identity.UserArn != "" {
		userID = request.RequestContext.Identity.UserArn
	} else if request.RequestContext.Identity.Caller != "" {
		userID = request.RequestContext.Identity.Caller
	}

	// Fallback: Try to get from Authorizer (Cognito or custom authorizer)
	if userID == "" {
		if claims, ok := request.RequestContext.Authorizer["claims"].(map[string]interface{}); ok {
			if sub, ok := claims["sub"].(string); ok {
				userID = sub
			}
		}
	}

	// Fallback: Use principal ID
	if userID == "" {
		if principalID, ok := request.RequestContext.Authorizer["principalId"].(string); ok {
			userID = principalID
		}
	}

	// If still no user ID, return error
	if userID == "" {
		return "", "", "", fmt.Errorf("unable to determine user identity from request")
	}

	// Check if user account info is cached in DynamoDB
	cached, err := getUserAccount(ctx, cfg, userID)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to query user account cache: %w", err)
	}

	// If cached, update last access and return
	if cached != nil {
		// Update last access asynchronously (don't block on this)
		go updateLastAccess(context.Background(), cfg, userID)

		// Use cached CLI IAM ARN if available, fallback to AWS account ID
		cliIamArn := cached.CliIamArn
		if cliIamArn == "" {
			cliIamArn = cached.AWSAccountID
		}

		return userID, cliIamArn, cached.AccountBase36, nil
	}

	// Not cached - detect account ID using STS
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get caller identity: %w", err)
	}

	if identity.Account == nil || identity.Arn == nil {
		return "", "", "", fmt.Errorf("incomplete identity response from STS")
	}

	userARN := aws.ToString(identity.Arn)
	accountID := aws.ToString(identity.Account)
	accountBase36 := intToBase36(accountID)

	// Determine identity type and extract CLI IAM ARN
	var cliIamArn string
	var identityType string

	if strings.Contains(userARN, ":user/") {
		// Direct IAM user (CLI)
		cliIamArn = userARN
		identityType = "cli"
	} else {
		// Cognito/assumed role (web)
		identityType = "web"

		// Extract email from Cognito claims
		email, err := extractEmailFromRequest(request)
		if err != nil {
			return "", "", "", fmt.Errorf("web users must have email: %w", err)
		}

		// Look up CLI IAM ARN by email
		cliIamArn, err = lookupCliIamArnByEmail(ctx, cfg, email)
		if err != nil {
			log.Printf("identity not linked for email %s: %v", email, err)
			return "", "", "", fmt.Errorf("identity not linked: link your CLI IAM user via 'spawn auth link'")
		}
	}

	// Create cache entry
	email := ""
	if identityType == "web" {
		email, _ = extractEmailFromRequest(request)
	}

	err = createUserAccountWithIdentity(ctx, cfg, userID, cliIamArn, identityType, accountID, accountBase36, email)
	if err != nil {
		// Log error but don't fail the request
		log.Printf("warning: failed to cache user account: %v", err)
	}

	return userID, cliIamArn, accountBase36, nil
}

// getUserFromCredentialsHeader validates credentials from X-AWS-Credentials header
// Header format: base64-encoded JSON with accessKeyId, secretAccessKey, sessionToken
func getUserFromCredentialsHeader(ctx context.Context, cfg aws.Config, request events.APIGatewayProxyRequest, credsHeader string) (string, string, string, error) {
	// Reject oversized headers before attempting decode.
	const maxCredentialHeaderBytes = 8192
	if len(credsHeader) > maxCredentialHeaderBytes {
		return "", "", "", fmt.Errorf("credentials header too large")
	}

	// Decode base64 credentials
	credsJSON, err := base64.StdEncoding.DecodeString(credsHeader)
	if err != nil {
		log.Printf("failed to decode credentials header: %v", err)
		return "", "", "", fmt.Errorf("failed to decode credentials header: %w", err)
	}

	var creds struct {
		AccessKeyID     string `json:"accessKeyId"`
		SecretAccessKey string `json:"secretAccessKey"`
		SessionToken    string `json:"sessionToken"`
	}

	if err := json.Unmarshal(credsJSON, &creds); err != nil {
		log.Printf("failed to parse credentials JSON: %v", err)
		return "", "", "", fmt.Errorf("failed to parse credentials JSON: %w", err)
	}

	// Validate credentials using STS GetCallerIdentity
	stsClient := sts.NewFromConfig(cfg, func(o *sts.Options) {
		o.Credentials = aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     creds.AccessKeyID,
				SecretAccessKey: creds.SecretAccessKey,
				SessionToken:    creds.SessionToken,
			}, nil
		})
	})

	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		log.Printf("STS GetCallerIdentity failed: %v", err)
		return "", "", "", fmt.Errorf("invalid credentials: %w", err)
	}

	log.Printf("credentials validated")

	if identity.Arn == nil || identity.Account == nil {
		return "", "", "", fmt.Errorf("incomplete identity response from STS")
	}

	userID := *identity.Arn
	accountID := *identity.Account
	accountBase36 := intToBase36(accountID)

	// Check cache
	cached, err := getUserAccount(ctx, cfg, userID)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to query user account cache: %w", err)
	}

	if cached != nil {
		go updateLastAccess(context.Background(), cfg, userID)

		// Use cached CLI IAM ARN if available, fallback to AWS account ID
		cliIamArn := cached.CliIamArn
		if cliIamArn == "" {
			cliIamArn = cached.AWSAccountID
		}

		return userID, cliIamArn, cached.AccountBase36, nil
	}

	// Determine identity type and extract CLI IAM ARN
	var cliIamArn string
	var identityType string

	if strings.Contains(userID, ":user/") {
		// Direct IAM user (CLI)
		cliIamArn = userID
		identityType = "cli"
	} else {
		// Cognito/assumed role (web)
		identityType = "web"

		// Extract email from Cognito claims in request
		email, err := extractEmailFromRequest(request)
		if err != nil {
			return "", "", "", fmt.Errorf("web users must have email: %w", err)
		}

		// Look up CLI IAM ARN by email
		cliIamArn, err = lookupCliIamArnByEmail(ctx, cfg, email)
		if err != nil {
			log.Printf("identity not linked for email %s: %v", email, err)
			return "", "", "", fmt.Errorf("identity not linked: link your CLI IAM user via 'spawn auth link'")
		}
	}

	// Create cache entry
	email := ""
	if identityType == "web" {
		email, _ = extractEmailFromRequest(request)
	}

	if err := createUserAccountWithIdentity(ctx, cfg, userID, cliIamArn, identityType, accountID, accountBase36, email); err != nil {
		log.Printf("warning: failed to cache user account: %v", err)
	}

	return userID, cliIamArn, accountBase36, nil
}

// extractEmailFromRequest extracts email from API Gateway request
// Relies on Cognito authorizer claims only; never trusts client-supplied headers.
func extractEmailFromRequest(request events.APIGatewayProxyRequest) (string, error) {
	// Try to get email from Cognito claims in authorizer context
	if claims, ok := request.RequestContext.Authorizer["claims"].(map[string]interface{}); ok {
		if email, ok := claims["email"].(string); ok && email != "" {
			return email, nil
		}
	}

	// Fallback: Check for email in identity context (less common)
	if request.RequestContext.Identity.CognitoIdentityID != "" {
		// For Cognito Identity Pool, email may be in authorizer claims
		if email, ok := request.RequestContext.Authorizer["email"].(string); ok && email != "" {
			return email, nil
		}
	}

	return "", fmt.Errorf("no email found in request")
}

// lookupCliIamArnByEmail queries spawn-user-accounts by email to find linked CLI IAM ARN
func lookupCliIamArnByEmail(ctx context.Context, cfg aws.Config, email string) (string, error) {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	queryInput := &dynamodb.QueryInput{
		TableName:              aws.String("spawn-user-accounts"),
		IndexName:              aws.String("email-index"),
		KeyConditionExpression: aws.String("email = :email"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":email": &types.AttributeValueMemberS{Value: email},
		},
	}

	result, err := dynamodbClient.Query(ctx, queryInput)
	if err != nil {
		return "", fmt.Errorf("failed to query by email: %w", err)
	}

	if len(result.Items) == 0 {
		return "", fmt.Errorf("no linked identity found for email: %s", email)
	}

	// Return cli_iam_arn from first matching record
	var user UserAccountRecord
	err = attributevalue.UnmarshalMap(result.Items[0], &user)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal user record: %w", err)
	}

	if user.CliIamArn == "" {
		return "", fmt.Errorf("user record has no cli_iam_arn: %s", email)
	}

	return user.CliIamArn, nil
}

// createUserAccountWithIdentity creates/updates user account record with identity mapping
func createUserAccountWithIdentity(ctx context.Context, cfg aws.Config, userID, cliIamArn, identityType, accountID, accountBase36, email string) error {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	now := time.Now().UTC().Format(time.RFC3339)

	item := map[string]types.AttributeValue{
		"user_id":        &types.AttributeValueMemberS{Value: userID},
		"cli_iam_arn":    &types.AttributeValueMemberS{Value: cliIamArn},
		"identity_type":  &types.AttributeValueMemberS{Value: identityType},
		"aws_account_id": &types.AttributeValueMemberS{Value: accountID},
		"account_base36": &types.AttributeValueMemberS{Value: accountBase36},
		"created_at":     &types.AttributeValueMemberS{Value: now},
		"last_access":    &types.AttributeValueMemberS{Value: now},
		"linked_at":      &types.AttributeValueMemberS{Value: now},
	}

	if email != "" {
		item["email"] = &types.AttributeValueMemberS{Value: email}
	}

	_, err := dynamodbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("spawn-user-accounts"),
		Item:      item,
	})

	return err
}
