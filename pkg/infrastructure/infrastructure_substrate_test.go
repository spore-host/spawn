package infrastructure

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/testutil"
)

const (
	testRegion    = "us-east-1"
	testAccountID = "123456789012"
)

// defaultInfraConfig returns an InfrastructureConfig that uses the standard
// default resource names (spawn-schedules, spawn-binaries-us-east-1, etc.).
func defaultInfraConfig() *config.InfrastructureConfig {
	return &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}
}

// createDynamoTable creates a single DynamoDB table in the emulator.
func createDynamoTable(t *testing.T, db *dynamodb.Client, name string) {
	t.Helper()
	_, err := db.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		t.Fatalf("createDynamoTable %s: %v", name, err)
	}
}

// createAllDynamoTables provisions all four tables the validator checks.
func createAllDynamoTables(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	for _, name := range []string{
		"spawn-schedules",
		"spawn-sweep-orchestration",
		"spawn-alerts",
		"spawn-alert-history",
	} {
		createDynamoTable(t, db, name)
	}
}

// createS3Bucket creates a single S3 bucket in the emulator.
func createS3Bucket(t *testing.T, client *s3.Client, name string) {
	t.Helper()
	_, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		t.Fatalf("createS3Bucket %s: %v", name, err)
	}
}

// createAllS3Buckets provisions both S3 buckets the validator checks.
func createAllS3Buckets(t *testing.T, client *s3.Client) {
	t.Helper()
	for _, name := range []string{
		"spawn-binaries-" + testRegion,
		"spawn-schedules-" + testRegion,
	} {
		createS3Bucket(t, client, name)
	}
}

// createLambdaFunction creates a stub Lambda function in the emulator.
func createLambdaFunction(t *testing.T, lc *lambdasvc.Client, name string) {
	t.Helper()
	_, err := lc.CreateFunction(context.Background(), &lambdasvc.CreateFunctionInput{
		FunctionName: aws.String(name),
		Role:         aws.String("arn:aws:iam::" + testAccountID + ":role/test-role"),
		Runtime:      lambdatypes.Runtime("python3.12"),
		Handler:      aws.String("index.handler"),
		Code: &lambdatypes.FunctionCode{
			ZipFile: []byte("placeholder"),
		},
	})
	if err != nil {
		t.Fatalf("createLambdaFunction %s: %v", name, err)
	}
}

// createAllLambdas provisions all four Lambda stubs the validator checks.
func createAllLambdas(t *testing.T, lc *lambdasvc.Client) {
	t.Helper()
	for _, name := range []string{
		"spawn-scheduler-handler",
		"spawn-sweep-orchestrator",
		"spawn-alert-handler",
		"spawn-dashboard-api",
	} {
		createLambdaFunction(t, lc, name)
	}
}

// newTestValidator returns a Validator pointed at the Substrate server.
func newTestValidator(env *testutil.TestEnv) *Validator {
	resolver := NewResolver(defaultInfraConfig(), testRegion, testAccountID)
	return NewValidator(resolver, env.AWSConfig)
}

func TestValidate_AllResourcesPresent(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createAllDynamoTables(t, env.DynamoClient())
	createAllS3Buckets(t, env.S3Client())
	createAllLambdas(t, env.LambdaClient())

	result, err := newTestValidator(env).ValidateAll(context.Background())
	if err != nil {
		t.Fatalf("ValidateAll() error = %v", err)
	}
	if !result.Valid {
		t.Errorf("Valid = false, want true; errors: %v", result.Errors)
	}
	if len(result.Errors) != 0 {
		t.Errorf("got %d error(s), want 0: %v", len(result.Errors), result.Errors)
	}
}

func TestValidate_MissingDynamoTable(t *testing.T) {
	env := testutil.SubstrateServer(t)
	// Provision all but one DynamoDB table.
	for _, name := range []string{
		"spawn-sweep-orchestration",
		"spawn-alerts",
		"spawn-alert-history",
	} {
		createDynamoTable(t, env.DynamoClient(), name)
	}
	createAllS3Buckets(t, env.S3Client())
	createAllLambdas(t, env.LambdaClient())

	result, err := newTestValidator(env).ValidateAll(context.Background())
	if err != nil {
		t.Fatalf("ValidateAll() error = %v", err)
	}
	if result.Valid {
		t.Error("Valid = true, want false when a DynamoDB table is missing")
	}
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "spawn-schedules") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning spawn-schedules; errors: %v", result.Errors)
	}
}

func TestValidate_MissingS3Bucket(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createAllDynamoTables(t, env.DynamoClient())
	// Omit spawn-binaries-us-east-1; only create schedules bucket.
	createS3Bucket(t, env.S3Client(), "spawn-schedules-"+testRegion)
	createAllLambdas(t, env.LambdaClient())

	result, err := newTestValidator(env).ValidateAll(context.Background())
	if err != nil {
		t.Fatalf("ValidateAll() error = %v", err)
	}
	if result.Valid {
		t.Error("Valid = true, want false when an S3 bucket is missing")
	}
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "spawn-binaries") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning spawn-binaries; errors: %v", result.Errors)
	}
}

func TestValidate_MissingLambda(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createAllDynamoTables(t, env.DynamoClient())
	createAllS3Buckets(t, env.S3Client())
	// Omit spawn-scheduler-handler.
	for _, name := range []string{
		"spawn-sweep-orchestrator",
		"spawn-alert-handler",
		"spawn-dashboard-api",
	} {
		createLambdaFunction(t, env.LambdaClient(), name)
	}

	result, err := newTestValidator(env).ValidateAll(context.Background())
	if err != nil {
		t.Fatalf("ValidateAll() error = %v", err)
	}
	if result.Valid {
		t.Error("Valid = true, want false when a Lambda function is missing")
	}
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "spawn-scheduler-handler") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning spawn-scheduler-handler; errors: %v", result.Errors)
	}
}

func TestValidate_EmptyInfra(t *testing.T) {
	env := testutil.SubstrateServer(t)
	// Provision nothing.

	result, err := newTestValidator(env).ValidateAll(context.Background())
	if err != nil {
		t.Fatalf("ValidateAll() error = %v", err)
	}
	if result.Valid {
		t.Error("Valid = true, want false when no resources are provisioned")
	}
	hasDynamo, hasS3, hasLambda := false, false, false
	for _, e := range result.Errors {
		if strings.Contains(e, "DynamoDB") {
			hasDynamo = true
		}
		if strings.Contains(e, "S3") {
			hasS3 = true
		}
		if strings.Contains(e, "Lambda") {
			hasLambda = true
		}
	}
	if !hasDynamo {
		t.Errorf("expected DynamoDB error in empty-infra result; errors: %v", result.Errors)
	}
	if !hasS3 {
		t.Errorf("expected S3 error in empty-infra result; errors: %v", result.Errors)
	}
	if !hasLambda {
		t.Errorf("expected Lambda error in empty-infra result; errors: %v", result.Errors)
	}
}

func TestValidate_Recommendations(t *testing.T) {
	env := testutil.SubstrateServer(t)
	// Provision nothing so recommendations are generated.

	v := newTestValidator(env)
	result, err := v.ValidateAll(context.Background())
	if err != nil {
		t.Fatalf("ValidateAll() error = %v", err)
	}
	if result.Valid {
		t.Skip("infrastructure unexpectedly valid — skipping recommendations check")
	}
	recs := v.GetRecommendations(result)
	if len(recs) == 0 {
		t.Error("GetRecommendations() returned empty slice for invalid infra")
	}
}
