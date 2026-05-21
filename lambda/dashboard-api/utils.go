package main

import (
	"encoding/json"
	"strconv"

	"github.com/aws/aws-lambda-go/events"
)

// CORS headers for API responses
var corsHeaders = map[string]string{
	"Access-Control-Allow-Origin":      "https://spore.host",
	"Access-Control-Allow-Methods":     "GET,POST,PUT,DELETE,OPTIONS",
	"Access-Control-Allow-Headers":     "Content-Type,Authorization,X-Amz-Date,X-Api-Key,X-Amz-Security-Token,X-AWS-Credentials,X-Team-ID",
	"Access-Control-Allow-Credentials": "true",
	"Content-Type":                     "application/json",
}

// successResponse creates a successful API Gateway response
func successResponse(data interface{}) (events.APIGatewayProxyResponse, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return errorResponse(500, "Failed to marshal response"), nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers:    corsHeaders,
		Body:       string(body),
	}, nil
}

// errorResponse creates an error API Gateway response
func errorResponse(statusCode int, message string) events.APIGatewayProxyResponse {
	response := APIResponse{
		Success: false,
		Error:   message,
	}

	body, _ := json.Marshal(response)

	return events.APIGatewayProxyResponse{
		StatusCode: statusCode,
		Headers:    corsHeaders,
		Body:       string(body),
	}
}

// intToBase36 converts a numeric string (AWS account ID) to base36
// Example: "942542972736" -> "c0zxr0ao"
func intToBase36(accountID string) string {
	// Parse account ID as integer
	num, err := strconv.ParseUint(accountID, 10, 64)
	if err != nil {
		// Fallback: return account ID as-is if parsing fails
		return accountID
	}

	// Convert to base36 (lowercase)
	return strconv.FormatUint(num, 36)
}

// getFullDNSName constructs the full DNS name for an instance
// Format: <name>.<account-base36>.spore.host
func getFullDNSName(instanceName, accountBase36 string) string {
	if instanceName == "" {
		return ""
	}
	return instanceName + "." + accountBase36 + ".spore.host"
}
