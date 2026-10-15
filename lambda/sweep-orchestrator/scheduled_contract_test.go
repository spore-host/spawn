package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/spore-host/spawn/pkg/sweep"
)

// The orchestrator's side of the contract with scheduler-handler (#1026).
//
// scheduler-handler builds the record, the staged params body and the invoke
// payload through pkg/sweep (NewScheduledSweepRecord, EncodeParams,
// OrchestratorEvent). This test feeds exactly those through the orchestrator's
// own decoding — the event type, the DynamoDB record type, splitParamsKey and
// the params type — so a change on either side that the other cannot consume
// fails here. Before the fix the handler wrote no record and its only key was a
// bare "schedules/<id>/params.yaml", which splitParamsKey reads as bucket
// "schedules".
func TestOrchestratorConsumesWhatAScheduledSweepProduces(t *testing.T) {
	const region = "us-east-1" // where the orchestrator, its table and its bucket live
	params := &sweep.ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0"},
		Params:   []map[string]interface{}{{"alpha": 1.0}, {"alpha": 2.0}},
	}
	built, err := sweep.NewScheduledSweepRecord(sweep.ScheduledSweep{
		SweepID:       "nightly-20261015-000000",
		SweepName:     "nightly",
		UserID:        "966362334030",
		ScheduleID:    "sched-1",
		AccountID:     "435415984226",
		Region:        "us-west-2",
		MaxConcurrent: 4,
		LaunchDelay:   "5s",
		ParamsBucket:  sweep.SweepsBucket(region),
		Params:        params,
	}, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("NewScheduledSweepRecord: %v", err)
	}

	// 1. The invoke payload decodes into the event the handler is started with.
	payload, err := json.Marshal(sweep.OrchestratorEvent{SweepID: built.SweepID})
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var ev SweepEvent
	if err := dec.Decode(&ev); err != nil {
		t.Fatalf("payload %s does not decode as SweepEvent: %v", payload, err)
	}
	if ev.SweepID != built.SweepID {
		t.Fatalf("event sweep_id = %q, want %q", ev.SweepID, built.SweepID)
	}

	// 2. The record survives DynamoDB into the type loadSweepState unmarshals.
	item, err := attributevalue.MarshalMap(built)
	if err != nil {
		t.Fatal(err)
	}
	var state SweepRecord
	if err := attributevalue.UnmarshalMap(item, &state); err != nil {
		t.Fatalf("record does not unmarshal as the orchestrator's SweepRecord: %v", err)
	}
	if state.SweepID != ev.SweepID {
		t.Errorf("record sweep_id %q is not the invoked one %q", state.SweepID, ev.SweepID)
	}
	if state.Status != "INITIALIZING" {
		t.Errorf("status = %q; the orchestrator only sets up a sweep from INITIALIZING", state.Status)
	}
	if state.AWSAccountID != "435415984226" {
		t.Errorf("aws_account_id = %q; createCrossAccountEC2Client assumes a role there", state.AWSAccountID)
	}
	if state.Region != "us-west-2" || state.MultiRegion {
		t.Errorf("region = %q multi=%v, want single-region us-west-2", state.Region, state.MultiRegion)
	}

	// 3. The params key parses to the bucket the handler staged into, at a key
	// the orchestrator's IAM role is granted.
	bucket, key, err := splitParamsKey(state.S3ParamsKey)
	if err != nil {
		t.Fatalf("splitParamsKey(%q): %v", state.S3ParamsKey, err)
	}
	if bucket != sweep.SweepsBucket(region) || key != sweep.ParamsObjectKey(built.SweepID) {
		t.Errorf("s3_params_key %q resolves to s3://%s/%s, want the staged s3://%s/%s",
			state.S3ParamsKey, bucket, key, sweep.SweepsBucket(region), sweep.ParamsObjectKey(built.SweepID))
	}
	grant := orchestratorS3Grant(t)
	if !iamResourceMatches(strings.ReplaceAll(grant, "${REGION}", region), bucket+"/"+key) {
		t.Errorf("s3://%s/%s is outside the orchestrator role's s3:GetObject grant %q", bucket, key, grant)
	}

	// 4. The staged body decodes as the params downloadParams returns.
	body, err := sweep.EncodeParams(params)
	if err != nil {
		t.Fatal(err)
	}
	var got ParamFileFormat
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&got); err != nil {
		t.Fatalf("staged params do not decode: %v", err)
	}
	if len(got.Params) != state.TotalParams || got.Defaults["ami"] != "ami-0123456789abcdef0" {
		t.Errorf("decoded params = %+v, want %d rows with the ami default", got, state.TotalParams)
	}
}

// The bare key a schedule record carries must NOT be accepted as a params
// location: this is the shape the handler would have forwarded, and it parses
// to the wrong bucket rather than failing. Pins splitParamsKey's reading so the
// contract test above cannot pass against a parser that ignores the bucket.
func TestSplitParamsKeyReadsTheBucketFromTheURI(t *testing.T) {
	b, k, err := splitParamsKey("s3://spawn-sweeps-us-east-1/sweeps/x/params.json")
	if err != nil || b != "spawn-sweeps-us-east-1" || k != "sweeps/x/params.json" {
		t.Errorf("got %q %q %v", b, k, err)
	}
	if b, _, _ := splitParamsKey("schedules/sched-1/params.yaml"); b == "spawn-schedules-us-east-1" {
		t.Errorf("a bare key resolved to a real bucket")
	}
}

// orchestratorS3Grant reads the s3:GetObject resource the orchestrator's role
// is created with. Anchored on the Resource assignment, not on the bucket name,
// so a comment mentioning it cannot satisfy the match.
func orchestratorS3Grant(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "setup-sweep-orchestrator-lambda-role.sh"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*"Resource":\s*"arn:aws:s3:::([^"]+)"`)
	m := re.FindAllSubmatch(data, -1)
	if len(m) != 1 {
		t.Fatalf("found %d s3 Resource grants in the orchestrator role script, want exactly 1", len(m))
	}
	return string(m[0][1])
}

// iamResourceMatches applies IAM's resource wildcards, where * spans "/" (unlike
// path.Match) and ? is one character.
func iamResourceMatches(pattern, resource string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(resource)
}
