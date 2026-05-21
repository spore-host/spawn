package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// TestCORSHeaders verifies the corsHeaders map has required CORS fields.
func TestCORSHeaders(t *testing.T) {
	required := []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Content-Type",
	}
	for _, key := range required {
		if v := corsHeaders[key]; v == "" {
			t.Errorf("corsHeaders[%q] is empty", key)
		}
	}
}

// TestErrorResponse verifies errorResponse returns correct status and body.
func TestErrorResponse(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		message    string
	}{
		{
			name:       "404 not found",
			statusCode: 404,
			message:    "Endpoint not found",
		},
		{
			name:       "401 unauthorized",
			statusCode: 401,
			message:    "authentication failed",
		},
		{
			name:       "400 bad request",
			statusCode: 400,
			message:    "team_id is required",
		},
		{
			name:       "415 unsupported media",
			statusCode: 415,
			message:    "Content-Type must be application/json",
		},
		{
			name:       "500 internal error",
			statusCode: 500,
			message:    "Failed to load AWS config",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := errorResponse(tc.statusCode, tc.message)

			if resp.StatusCode != tc.statusCode {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tc.statusCode)
			}

			// Body must be valid JSON.
			var body APIResponse
			if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}

			if body.Success {
				t.Error("error response has Success=true")
			}
			if body.Error != tc.message {
				t.Errorf("Error = %q, want %q", body.Error, tc.message)
			}

			// CORS headers must be present.
			if resp.Headers["Access-Control-Allow-Origin"] == "" {
				t.Error("missing Access-Control-Allow-Origin header")
			}
		})
	}
}

// TestIntToBase36 verifies account ID encoding.
func TestIntToBase36(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		want      string
	}{
		{
			name:      "example from comment",
			accountID: "942542972736",
			want:      "c0zxr0ao",
		},
		{
			name:      "zero",
			accountID: "0",
			want:      "0",
		},
		{
			name:      "small number",
			accountID: "36",
			want:      "10",
		},
		{
			name:      "deterministic",
			accountID: "123456789012",
			want:      intToBase36("123456789012"), // self-consistency
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := intToBase36(tc.accountID)
			if got != tc.want {
				t.Errorf("intToBase36(%q) = %q, want %q", tc.accountID, got, tc.want)
			}
		})
	}

	t.Run("invalid input returns input unchanged", func(t *testing.T) {
		got := intToBase36("not-a-number")
		if got != "not-a-number" {
			t.Errorf("intToBase36(%q) = %q, want fallback %q", "not-a-number", got, "not-a-number")
		}
	})
}

// TestGetFullDNSName verifies DNS name construction.
func TestGetFullDNSName(t *testing.T) {
	tests := []struct {
		name          string
		instanceName  string
		accountBase36 string
		want          string
	}{
		{
			name:          "basic",
			instanceName:  "worker",
			accountBase36: "abc123",
			want:          "worker.abc123.spore.host",
		},
		{
			name:          "hyphenated name",
			instanceName:  "gpu-node-01",
			accountBase36: "c0zxr0ao",
			want:          "gpu-node-01.c0zxr0ao.spore.host",
		},
		{
			name:          "empty name returns empty",
			instanceName:  "",
			accountBase36: "abc123",
			want:          "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getFullDNSName(tc.instanceName, tc.accountBase36)
			if got != tc.want {
				t.Errorf("getFullDNSName(%q, %q) = %q, want %q",
					tc.instanceName, tc.accountBase36, got, tc.want)
			}
		})
	}
}

// TestHandlerOPTIONS verifies CORS preflight is handled without AWS calls.
func TestHandlerOPTIONS(t *testing.T) {
	req := events.APIGatewayProxyRequest{
		HTTPMethod: "OPTIONS",
		Path:       "/api/instances",
	}

	// handler() loads AWS config after the OPTIONS check, so OPTIONS must
	// return 200 before any AWS call is attempted.
	resp, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler OPTIONS returned error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("OPTIONS status = %d, want 200", resp.StatusCode)
	}
	if resp.Headers["Access-Control-Allow-Origin"] == "" {
		t.Error("OPTIONS missing Access-Control-Allow-Origin")
	}
	if resp.Headers["Access-Control-Allow-Methods"] == "" {
		t.Error("OPTIONS missing Access-Control-Allow-Methods")
	}
}

// TestHandlerUnknownPath verifies 404 for unknown routes.
// The handler will fail auth before reaching the router, so we expect 401 or 404.
// We test that the response is one of these and the body is valid JSON.
func TestHandlerUnknownPath(t *testing.T) {
	req := events.APIGatewayProxyRequest{
		HTTPMethod: "GET",
		Path:       "/api/does-not-exist",
	}

	resp, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	// Should be 401 (auth fails first) or 404 (unknown path) or 500 (config failure)
	if resp.StatusCode != 401 && resp.StatusCode != 404 && resp.StatusCode != 500 {
		t.Errorf("got status %d, want 401/404/500", resp.StatusCode)
	}

	var body APIResponse
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if body.Success {
		t.Error("expected Success=false for unknown path")
	}
}

// TestHandlerMissingTeamID verifies that requests without auth return non-200.
func TestHandlerMissingTeamID(t *testing.T) {
	req := events.APIGatewayProxyRequest{
		HTTPMethod: "GET",
		Path:       "/teams",
		RequestContext: events.APIGatewayProxyRequestContext{
			Identity: events.APIGatewayRequestIdentity{
				UserArn: "arn:aws:iam::123456789012:user/test-user",
			},
		},
	}

	resp, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	// Without valid AWS creds, will get 401, 500, or 400 — all are non-2xx and have JSON body.
	if resp.StatusCode == 200 {
		t.Error("expected non-200 for unauthenticated request")
	}
	var body APIResponse
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
}

// TestHandlerStrataGetCatalog verifies GET /api/strata/catalog returns the formation list.
func TestHandlerStrataGetCatalog(t *testing.T) {
	resp, err := handleStrataGetCatalog()
	if err != nil {
		t.Fatalf("handleStrataGetCatalog returned error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Success    bool              `json:"success"`
		Formations []StrataFormation `json:"formations"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if !body.Success {
		t.Error("expected success=true")
	}
	if len(body.Formations) == 0 {
		t.Error("expected non-empty formations list")
	}
	for _, f := range body.Formations {
		if f.Name == "" {
			t.Error("formation has empty name")
		}
		if f.DisplayName == "" {
			t.Errorf("formation %q has empty display_name", f.Name)
		}
	}
}

// TestHandlerStrataResolve_InvalidBody verifies POST /api/strata/resolve rejects bad input.
func TestHandlerStrataResolve_InvalidBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "invalid JSON", body: "{not json}"},
		{name: "missing formation", body: `{"arch":"x86_64"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := handleStrataResolve(context.Background(), tc.body)
			if err != nil {
				t.Fatalf("handleStrataResolve returned error: %v", err)
			}
			if resp.StatusCode != 400 {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// TestSuccessResponse verifies successResponse structure.
func TestSuccessResponse(t *testing.T) {
	data := APIResponse{
		Success:        true,
		TotalInstances: 3,
	}
	resp, err := successResponse(data)
	if err != nil {
		t.Fatalf("successResponse returned error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", resp.Headers["Content-Type"])
	}
	if !strings.Contains(resp.Body, `"success":true`) {
		t.Errorf("body %q missing success:true", resp.Body)
	}
}
