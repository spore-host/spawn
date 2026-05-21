package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/scttfrdmn/strata/pkg/strata"
	"github.com/scttfrdmn/strata/spec"
)

// handler is the main Lambda handler
func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// Extract path and method
	path := request.Path
	method := request.HTTPMethod

	log.Printf("request: method=%s path=%s", method, path)

	// Handle OPTIONS for CORS preflight
	if method == "OPTIONS" {
		return events.APIGatewayProxyResponse{
			StatusCode: 200,
			Headers:    corsHeaders,
			Body:       "",
		}, nil
	}

	// OAuth routes bypass authentication — they're called by Slack's redirect,
	// not by an authenticated spore.host user.
	if path == "/api/slack/oauth" && method == "GET" {
		return handleSlackOAuthRedirect(request)
	}

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return errorResponse(500, "Failed to load AWS config"), nil
	}

	if path == "/api/slack/oauth/callback" && method == "GET" {
		return handleSlackOAuthCallback(ctx, cfg, request)
	}

	if path == "/api/slack/token/rotate" && method == "POST" {
		return handleSlackTokenRotate(ctx, cfg, request)
	}

	// Extract user identity and account info
	userID, cliIamArn, accountBase36, err := getUserFromRequest(ctx, cfg, request)
	if err != nil {
		log.Printf("authentication failed: %v", err)
		return errorResponse(401, "authentication failed"), nil
	}

	log.Printf("authentication successful")

	// Extract optional team context
	teamID := request.Headers["X-Team-ID"]
	if teamID == "" {
		teamID = request.Headers["x-team-id"]
	}

	// Route to appropriate handler
	// teamPathParts parses /teams[/{team_id}[/members[/{member_arn}]]]
	// Returns (teamID, subpath) where subpath is "" | "members" | "members/{arn}"
	teamPathParts := func() (string, string) {
		if !strings.HasPrefix(path, "/teams/") {
			return "", ""
		}
		rest := strings.TrimPrefix(path, "/teams/")
		idx := strings.Index(rest, "/")
		if idx < 0 {
			return rest, ""
		}
		return rest[:idx], rest[idx+1:]
	}

	switch {
	// Team endpoints
	case path == "/teams" && method == "POST":
		if ct := request.Headers["content-type"]; !strings.HasPrefix(strings.ToLower(ct), "application/json") {
			return errorResponse(415, "Content-Type must be application/json"), nil
		}
		return handleCreateTeam(ctx, cfg, request.Body, cliIamArn)
	case path == "/teams" && method == "GET":
		return handleListMyTeams(ctx, cfg, cliIamArn)
	case strings.HasPrefix(path, "/teams/") && method == "GET":
		tid, sub := teamPathParts()
		if tid == "" {
			return errorResponse(400, "team_id is required"), nil
		}
		_ = sub
		return handleGetTeam(ctx, cfg, tid, cliIamArn)
	case strings.HasPrefix(path, "/teams/") && strings.HasSuffix(path, "/members") && method == "POST":
		tid, _ := teamPathParts()
		if tid == "" {
			return errorResponse(400, "team_id is required"), nil
		}
		if ct := request.Headers["content-type"]; !strings.HasPrefix(strings.ToLower(ct), "application/json") {
			return errorResponse(415, "Content-Type must be application/json"), nil
		}
		return handleAddMember(ctx, cfg, tid, cliIamArn, request.Body)
	case strings.HasPrefix(path, "/teams/") && strings.Contains(path, "/members/") && method == "DELETE":
		tid, sub := teamPathParts()
		if tid == "" {
			return errorResponse(400, "team_id is required"), nil
		}
		mArn, _ := url.PathUnescape(strings.TrimPrefix(sub, "members/"))
		if mArn == "" {
			return errorResponse(400, "member_arn is required"), nil
		}
		return handleRemoveMember(ctx, cfg, tid, cliIamArn, mArn)
	case strings.HasPrefix(path, "/teams/") && method == "DELETE":
		tid, _ := teamPathParts()
		if tid == "" {
			return errorResponse(400, "team_id is required"), nil
		}
		return handleDeleteTeam(ctx, cfg, tid, cliIamArn)

	case path == "/api/instances" && method == "GET":
		return handleListInstances(ctx, cfg, cliIamArn, teamID)

	case path == "/api/instances/" && method == "GET":
		// Extract instance ID from path
		instanceID := request.PathParameters["id"]
		if instanceID == "" {
			return errorResponse(400, "Instance ID is required"), nil
		}
		return handleGetInstance(ctx, cfg, instanceID, cliIamArn)

	case path == "/api/sweeps" && method == "GET":
		return handleListSweeps(ctx, cfg, cliIamArn, teamID)

	case path == "/api/sweeps/" && method == "GET":
		// Extract sweep ID from path
		sweepID := request.PathParameters["id"]
		if sweepID == "" {
			return errorResponse(400, "Sweep ID is required"), nil
		}
		return handleGetSweep(ctx, cfg, sweepID, cliIamArn)

	case path == "/api/sweeps//cancel" && method == "POST":
		// Extract sweep ID from path
		sweepID := request.PathParameters["id"]
		if sweepID == "" {
			return errorResponse(400, "Sweep ID is required"), nil
		}
		return handleCancelSweep(ctx, cfg, sweepID, cliIamArn)

	case path == "/api/sweeps/cleanup" && method == "POST":
		return handleCleanupSweeps(ctx, cfg, request.Body, cliIamArn)

	case path == "/api/autoscale-groups" && method == "GET":
		return handleListAutoscaleGroups(ctx, cfg, cliIamArn, teamID)

	case path == "/api/autoscale-groups/" && method == "GET":
		// Extract group ID from path
		groupID := request.PathParameters["id"]
		if groupID == "" {
			return errorResponse(400, "Autoscale group ID is required"), nil
		}
		return handleGetAutoscaleGroup(ctx, cfg, groupID, cliIamArn)

	case path == "/api/autoscale-groups//pause" && method == "POST":
		groupID := request.PathParameters["id"]
		if groupID == "" {
			return errorResponse(400, "Autoscale group ID is required"), nil
		}
		return handlePauseAutoscaleGroup(ctx, cfg, groupID, cliIamArn)

	case path == "/api/autoscale-groups//resume" && method == "POST":
		groupID := request.PathParameters["id"]
		if groupID == "" {
			return errorResponse(400, "Autoscale group ID is required"), nil
		}
		return handleResumeAutoscaleGroup(ctx, cfg, groupID, cliIamArn)

	case path == "/api/autoscale-groups/" && method == "DELETE":
		groupID := request.PathParameters["id"]
		if groupID == "" {
			return errorResponse(400, "Autoscale group ID is required"), nil
		}
		return handleTerminateAutoscaleGroup(ctx, cfg, groupID, cliIamArn)

	case path == "/api/instances/" && method == "DELETE":
		instanceID := request.PathParameters["id"]
		if instanceID == "" {
			return errorResponse(400, "Instance ID is required"), nil
		}
		return handleTerminateInstance(ctx, cfg, instanceID, cliIamArn)

	case path == "/api/cost-summary" && method == "GET":
		return handleGetCostSummary(ctx, cfg, cliIamArn)

	case path == "/api/cost-history" && method == "GET":
		days := 30
		if d := request.QueryStringParameters["days"]; d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		return handleGetCostHistory(ctx, cfg, days, cliIamArn)

	case path == "/api/alert-preferences" && method == "GET":
		return handleGetAlertPreferences(ctx, cfg, cliIamArn)

	case path == "/api/alert-preferences" && method == "POST":
		return handleSaveAlertPreferences(ctx, cfg, request.Body, cliIamArn)

	case path == "/api/user/profile" && method == "GET":
		return handleGetUserProfile(ctx, cfg, userID, cliIamArn, accountBase36)

	case path == "/api/ws-token" && method == "POST":
		return handleGetWSToken(ctx, cfg, cliIamArn)

	case path == "/api/watches" && method == "GET":
		return handleListWatches(ctx, cfg, cliIamArn)

	case strings.HasPrefix(path, "/api/watches/history") && method == "GET":
		return handleWatchHistory(ctx, cfg, cliIamArn)

	case strings.HasPrefix(path, "/api/watches/") && method == "GET":
		watchID := strings.TrimPrefix(path, "/api/watches/")
		if watchID == "" {
			return errorResponse(400, "Watch ID is required"), nil
		}
		return handleGetWatch(ctx, cfg, watchID, cliIamArn)

	case path == "/api/strata/catalog" && method == "GET":
		return handleStrataGetCatalog()

	case path == "/api/strata/resolve" && method == "POST":
		return handleStrataResolve(ctx, request.Body)

	default:
		return errorResponse(404, "Endpoint not found"), nil
	}
}

// handleListInstances handles GET /api/instances
func handleListInstances(ctx context.Context, cfg aws.Config, cliIamArn, teamID string) (events.APIGatewayProxyResponse, error) {
	startTime := time.Now()

	// Validate team membership before querying team-scoped instances.
	if teamID != "" {
		if _, err := resolveTeamContext(ctx, cfg, teamID, cliIamArn); err != nil {
			return errorResponse(403, "access denied"), nil
		}
	}

	// Query all regions in parallel (filtered by IAM user for per-user isolation)
	instances, err := listInstances(ctx, cfg, cliIamArn, teamID)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to list instances: %v", err)), nil
	}

	elapsed := time.Since(startTime)
	log.Printf("listed %d instances across %d regions in %v", len(instances), len(awsRegions), elapsed)

	// Build response
	response := APIResponse{
		Success:        true,
		RegionsQueried: awsRegions,
		TotalInstances: len(instances),
		Instances:      instances,
	}

	return successResponse(response)
}

// handleGetInstance handles GET /api/instances/{id}
func handleGetInstance(ctx context.Context, cfg aws.Config, instanceID, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	// Get single instance (with per-user isolation check)
	instance, err := getInstance(ctx, cfg, instanceID, cliIamArn)
	if err != nil {
		return errorResponse(404, fmt.Sprintf("Instance not found: %v", err)), nil
	}

	// Build response
	response := APIResponse{
		Success:  true,
		Instance: instance,
	}

	return successResponse(response)
}

// handleGetUserProfile handles GET /api/user/profile
func handleGetUserProfile(ctx context.Context, cfg aws.Config, userID, cliIamArn, accountBase36 string) (events.APIGatewayProxyResponse, error) {
	// Get user profile from DynamoDB
	cached, err := getUserAccount(ctx, cfg, userID)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to get user profile: %v", err)), nil
	}

	var profile UserProfile
	if cached != nil {
		createdAt, _ := time.Parse(time.RFC3339, cached.CreatedAt)
		lastAccess, _ := time.Parse(time.RFC3339, cached.LastAccess)

		profile = UserProfile{
			UserID:        cached.UserID,
			AWSAccountID:  cached.AWSAccountID,
			AccountBase36: cached.AccountBase36,
			Email:         cached.Email,
			CreatedAt:     createdAt,
			LastAccess:    lastAccess,
		}
	} else {
		// No cache entry, return detected info
		profile = UserProfile{
			UserID:        userID,
			AWSAccountID:  cliIamArn,
			AccountBase36: accountBase36,
			CreatedAt:     time.Now(),
			LastAccess:    time.Now(),
		}
	}

	// Build response
	response := APIResponse{
		Success: true,
		User:    &profile,
	}

	return successResponse(response)
}

// StrataFormation describes a software formation available in the catalog.
type StrataFormation struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

// StrataResolveRequest is the body for POST /api/strata/resolve.
type StrataResolveRequest struct {
	Formation string `json:"formation"`
	Arch      string `json:"arch"` // x86_64 | arm64
	OS        string `json:"os"`   // al2023 | ubuntu24 | rocky9
}

// StrataResolveResponse is returned by POST /api/strata/resolve.
type StrataResolveResponse struct {
	LockfileURI string `json:"lockfile_uri"`
}

var strataCatalog = []StrataFormation{
	{Name: "r-research@2024.03", DisplayName: "R + Quarto Workstation", Description: "R 4.3, Quarto, tidyverse, ggplot2, BiocManager"},
	{Name: "python-ml@2024.03", DisplayName: "Python ML (JupyterLab)", Description: "Python 3.11, JupyterLab, PyTorch, scikit-learn, pandas"},
	{Name: "hpc-mpi@2024.03", DisplayName: "HPC / MPI Cluster", Description: "OpenMPI 4.1, FFTW, ScaLAPACK, HDF5"},
	{Name: "genomics@2024.03", DisplayName: "Genomics", Description: "Python 3.11, samtools, bcftools, bwa, netcdf4"},
	{Name: "cuda-ml@2024.03", DisplayName: "CUDA + Python ML", Description: "CUDA 12.2, PyTorch GPU, cuDNN, tensorboard"},
}

// handleStrataGetCatalog handles GET /api/strata/catalog.
func handleStrataGetCatalog() (events.APIGatewayProxyResponse, error) {
	type catalogResponse struct {
		Success    bool              `json:"success"`
		Formations []StrataFormation `json:"formations"`
	}
	return successResponse(catalogResponse{Success: true, Formations: strataCatalog})
}

// handleStrataResolve handles POST /api/strata/resolve.
func handleStrataResolve(ctx context.Context, body string) (events.APIGatewayProxyResponse, error) {
	var req StrataResolveRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil || req.Formation == "" {
		return errorResponse(400, "invalid request body: formation required"), nil
	}

	registryURL := "s3://strata-registry"
	uri, err := resolveStrataFormation(ctx, req.Formation, req.OS, registryURL)
	if err != nil {
		log.Printf("strata resolve error: %v", err)
		return errorResponse(500, fmt.Sprintf("strata resolve failed: %v", err)), nil
	}

	return successResponse(StrataResolveResponse{LockfileURI: uri})
}

// resolveStrataFormation resolves a formation name to an S3 lockfile URI.
func resolveStrataFormation(ctx context.Context, formation, os, registry string) (string, error) {
	if os == "" {
		os = "al2023"
	}

	c, err := strata.NewClient(ctx, strata.Options{RegistryURL: registry})
	if err != nil {
		return "", fmt.Errorf("new client: %w", err)
	}

	profile := &spec.Profile{
		Name:     formation,
		Base:     spec.BaseRef{OS: os},
		Software: []spec.SoftwareRef{{Formation: formation}},
	}

	lf, err := c.Resolve(ctx, profile, strata.ResolveOptions{})
	if err != nil {
		return "", fmt.Errorf("resolve: %w", err)
	}

	uri, err := c.UploadLockfile(ctx, lf)
	if err != nil {
		return "", fmt.Errorf("upload lockfile: %w", err)
	}
	return uri, nil
}

// main is the entry point for the Lambda function
func main() {
	lambda.Start(handler)
}
