package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
)

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	method := req.RequestContext.HTTP.Method
	path := req.RequestContext.HTTP.Path

	// Twilio SMS webhook — no API key required, authenticated via Twilio signature
	if method == "POST" && path == "/v1/sms/incoming" {
		return handleSMSIncoming(ctx, req)
	}

	// All other routes require an API key
	apiKey := req.Headers["x-api-key"]
	if apiKey == "" {
		return errResp(http.StatusUnauthorized, "X-API-Key header required"), nil
	}
	principal, err := validateAPIKey(ctx, apiKey)
	if err != nil {
		return errResp(http.StatusUnauthorized, "invalid API key"), nil
	}

	// Load AWS config using the caller's configured region
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return errResp(http.StatusInternalServerError, "AWS config error"), nil
	}
	_ = cfg // used by sub-handlers

	log.Printf("%s %s principal=%s", method, path, principal.Project)

	// Route
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// /v1/...
	if len(parts) < 2 || parts[0] != "v1" {
		return errResp(http.StatusNotFound, "not found"), nil
	}

	switch {
	// GET /v1/instances
	case method == "GET" && len(parts) == 2 && parts[1] == "instances":
		return handleListInstances(ctx, cfg, req, principal)

	// POST /v1/instances  (launch)
	case method == "POST" && len(parts) == 2 && parts[1] == "instances":
		return handleLaunch(ctx, cfg, req, principal)

	// GET /v1/instances/{id}
	case method == "GET" && len(parts) == 3 && parts[1] == "instances":
		return handleGetInstance(ctx, cfg, parts[2], principal)

	// POST /v1/instances/{id}/{action}
	case method == "POST" && len(parts) == 4 && parts[1] == "instances":
		return handleInstanceAction(ctx, cfg, parts[2], parts[3], req, principal)

	// GET /v1/search
	case method == "GET" && len(parts) == 2 && parts[1] == "search":
		return handleSearch(ctx, cfg, req, principal)

	// GET /v1/spot
	case method == "GET" && len(parts) == 2 && parts[1] == "spot":
		return handleSpot(ctx, cfg, req, principal)

	// GET /v1/quota
	case method == "GET" && len(parts) == 2 && parts[1] == "quota":
		return handleQuota(ctx, cfg, req, principal)

	// POST /v1/notifications/register  DELETE /v1/notifications/register
	case (method == "POST" || method == "DELETE") && len(parts) == 3 &&
		parts[1] == "notifications" && parts[2] == "register":
		return handleNotificationRegister(ctx, method, req, principal)

	default:
		return errResp(http.StatusNotFound, fmt.Sprintf("no route for %s %s", method, path)), nil
	}
}

func errResp(status int, msg string) events.APIGatewayV2HTTPResponse {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}
}

func jsonResp(status int, v any) events.APIGatewayV2HTTPResponse {
	body, _ := json.Marshal(v)
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}
}
