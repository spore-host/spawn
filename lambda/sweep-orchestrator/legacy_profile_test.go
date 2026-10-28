package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	smithy "github.com/aws/smithy-go"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// missingProfileMsg is EC2's message when RunInstances names an instance
// profile that does not exist in the account, or has not reached EC2 yet (the
// shape pkg/aws's #572 retry matches).
const missingProfileMsg = "Value (" + spawnaws.SporedInstanceProfileName + ") for parameter iamInstanceProfile.name is invalid. " +
	"Invalid IAM Instance Profile name"

// profileRejectingEC2 is an EC2 transport that answers every RunInstances with
// EC2's invalid-profile error, as an EC2 query-protocol error response, and
// records the instance type each one asked for.
type profileRejectingEC2 struct {
	mu    sync.Mutex
	types []string
}

func (f *profileRejectingEC2) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(body), "Action=RunInstances") {
		return nil, errors.New("no AWS in tests")
	}
	f.mu.Lock()
	for _, kv := range strings.Split(string(body), "&") {
		if v, ok := strings.CutPrefix(kv, "InstanceType="); ok {
			f.types = append(f.types, v)
		}
	}
	f.mu.Unlock()
	xml := `<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>InvalidParameterValue</Code><Message>` +
		missingProfileMsg + `</Message></Error></Errors><RequestID>req-1369</RequestID></Response>`
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"text/xml;charset=UTF-8"}},
		Body:       io.NopCloser(strings.NewReader(xml)),
		Request:    req,
	}, nil
}

func (f *profileRejectingEC2) runs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.types...)
}

// profileHarness wires the launch paths to fakes: an EC2 client that rejects
// the profile, DynamoDB that fails at once (the availability records), and a
// record save that does nothing. It returns the EC2 fake and the client, and
// waits, at cleanup, for every availability record to have run.
func profileHarness(t *testing.T) (*profileRejectingEC2, *ec2.Client) {
	t.Helper()
	releaseOrchestrator(t, "0.147.1")
	dynamo := make(dynamoSignal, 256)
	prevDynamo, prevSave := dynamodbClient, saveProgress
	ec2Fake := &profileRejectingEC2{}
	t.Cleanup(func() {
		// One availability record per RunInstances reads dynamodbClient from
		// a goroutine; wait for each before restoring it.
		for range ec2Fake.runs() {
			select {
			case <-dynamo:
			case <-time.After(30 * time.Second):
				t.Error("a launch's availability record never ran")
			}
		}
		dynamodbClient, saveProgress = prevDynamo, prevSave
	})
	dynamodbClient = dynamodb.New(dynamodb.Options{
		Region: "us-east-1", Credentials: awssdk.AnonymousCredentials{},
		HTTPClient: &http.Client{Transport: dynamo}, RetryMaxAttempts: 1,
	})
	saveProgress = func(context.Context, *SweepRecord) error { return nil }
	client := ec2.New(ec2.Options{
		Region: "us-east-1", Credentials: awssdk.AnonymousCredentials{},
		HTTPClient: &http.Client{Transport: ec2Fake}, RetryMaxAttempts: 1,
	})
	return ec2Fake, client
}

// legacyNoRoleRows is n members' params as a v0.146.0 CLI uploads them with
// no IAM flags (no iam_role), each allowed two instance types, so a member
// whose error were taken for capacity would try the second.
func legacyNoRoleRows(n int) *ParamFileFormat {
	pf := &ParamFileFormat{Defaults: map[string]interface{}{}}
	for i := 0; i < n; i++ {
		row := v0146Params(false, "1h")
		delete(row, "iam_role")
		row["instance_type"] = "t3.micro|t3.small"
		pf.Params = append(pf.Params, row)
	}
	return pf
}

// cliRenderedRows is n members' params as a current CLI uploads them, naming
// the profile that CLI ensured.
func cliRenderedRows(t *testing.T, n int) *ParamFileFormat {
	pf := &ParamFileFormat{Defaults: map[string]interface{}{}}
	for i := 0; i < n; i++ {
		pf.Params = append(pf.Params, map[string]interface{}{
			userDataParam: "x", tagsParam: cliTags(nil), launchSettingsParam: cliSettings(t, nil),
			"iam_role": spawnaws.SporedInstanceProfileName, "instance_type": "t3.micro|t3.small",
		})
	}
	return pf
}

// The remedies the errors must name, by the sweep's source.
var (
	rerunRemedy    = "`spawn launch --param-file <params-file>`"
	scheduleRemedy = "`spawn schedule cancel sched-7`, then `spawn schedule create <params-file>`"
)

// TestLegacyMemberMissingProfileStopsTheSweep is #1369's gate on the
// single-region path. A legacy member with no iam_role launches with
// spored-instance-profile; when EC2 says the account has none, the member's
// row and the sweep's error name the spawn command that creates it, the
// member's other instance type is not tried (it is not capacity), the members
// after it are not attempted, and the sweep finishes FAILED.
func TestLegacyMemberMissingProfileStopsTheSweep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  func(*SweepRecord)
		remedy string
	}{
		{"cli sweep", func(*SweepRecord) {}, rerunRemedy},
		{"scheduled sweep", func(s *SweepRecord) { s.Source, s.ScheduleID = "scheduled", "sched-7" }, scheduleRemedy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ec2Fake, client := profileHarness(t)
			state := legacyState()
			state.TotalParams = 5
			tc.state(state)

			launchBatch(context.Background(), client, state, legacyNoRoleRows(5), 5, 0)

			if got := ec2Fake.runs(); len(got) != 1 || got[0] != "t3.micro" {
				t.Fatalf("RunInstances calls = %v, want one, for t3.micro: the members after the first, or the "+
					"first's other instance type, were attempted", got)
			}
			assertStoppedWithRemedy(t, state, 0, 4, tc.remedy)
			if state.NextToLaunch != state.TotalParams {
				t.Errorf("NextToLaunch = %d, want %d: the polling loop would go on launching", state.NextToLaunch, state.TotalParams)
			}
		})
	}
}

// TestLegacyMemberMissingProfileStopsTheMultiRegionSweep is the same gate on
// the multi-region path: the queues of every region are drained, not only the
// failing member's.
func TestLegacyMemberMissingProfileStopsTheMultiRegionSweep(t *testing.T) {
	ec2Fake, client := profileHarness(t)
	state := legacyState()
	state.TotalParams = 5
	state.MultiRegion = true
	// eu-west-1 has work but no client, so it never launches: its queue is
	// still drained when us-east-1's first member stops the sweep.
	state.RegionStatus = map[string]*RegionProgress{
		"us-east-1": {NextToLaunch: []int{0, 1, 2}},
		"eu-west-1": {NextToLaunch: []int{3, 4}},
	}
	orch := &RegionalOrchestrator{ec2Clients: map[string]*ec2.Client{"us-east-1": client}}

	if err := launchAcrossRegions(context.Background(), orch, state, legacyNoRoleRows(5), map[string]int{}, 5, 0); err != nil {
		t.Fatalf("launchAcrossRegions: %v", err)
	}

	if got := ec2Fake.runs(); len(got) != 1 || got[0] != "t3.micro" {
		t.Fatalf("RunInstances calls = %v, want one, for t3.micro", got)
	}
	assertStoppedWithRemedy(t, state, 0, 4, rerunRemedy)
	for r, rs := range state.RegionStatus {
		if len(rs.NextToLaunch) != 0 {
			t.Errorf("region %s still queues %v: the multi-region loop would launch them", r, rs.NextToLaunch)
		}
	}
}

// assertStoppedWithRemedy checks the record a stopped sweep leaves: one
// failed row, for failedIndex, naming the remedy and EC2's error; a sweep
// error naming the remedy and how many members were not attempted; and a
// FAILED finish.
func assertStoppedWithRemedy(t *testing.T, state *SweepRecord, failedIndex, notAttempted int, remedy string) {
	t.Helper()
	if state.Failed != 1 || state.Launched != 0 || len(state.Instances) != 1 {
		t.Fatalf("failed=%d launched=%d rows=%d, want one failed row", state.Failed, state.Launched, len(state.Instances))
	}
	row := state.Instances[0]
	if row.Index != failedIndex || row.State != "failed" {
		t.Errorf("row = %+v, want param set %d failed", row, failedIndex)
	}
	for _, want := range []string{remedy, spawnaws.SporedInstanceProfileName, sporedProfileFixedIn, "Invalid IAM Instance Profile name"} {
		if !strings.Contains(row.ErrorMessage, want) {
			t.Errorf("row error does not name %q:\n%s", want, row.ErrorMessage)
		}
	}
	for _, want := range []string{remedy, sporedProfileFixedIn, fmt.Sprintf("the %d not yet attempted", notAttempted)} {
		if !strings.Contains(state.ErrorMessage, want) {
			t.Errorf("sweep error does not name %q:\n%s", want, state.ErrorMessage)
		}
	}
	if got := abortedStatus(state, completionStatus(state.Launched, state.Failed)); got != "FAILED" {
		t.Errorf("finished status = %s, want FAILED", got)
	}
}

// A stopped sweep is FAILED even when members launched before the one that
// stopped it: most of it never ran.
func TestStoppedSweepIsFailedWhateverLaunched(t *testing.T) {
	state := legacyState()
	lpe := &legacyProfileError{paramIndex: 3, remedy: legacyProfileRemedy(state), err: errors.New("x")}
	if !abortForLegacyProfile(state, fmt.Errorf("run instances failed: %w", lpe), 2) {
		t.Fatal("a wrapped legacyProfileError did not stop the sweep")
	}
	if got := abortedStatus(state, completionStatus(3, 1)); got != "FAILED" {
		t.Errorf("status = %s, want FAILED", got)
	}
	if got := abortedStatus(legacyState(), completionStatus(3, 1)); got != "COMPLETED" {
		t.Errorf("a sweep that was not stopped: status = %s, want completionStatus's COMPLETED", got)
	}
}

// TestCLIRenderedMemberMissingProfileKeepsGoing pins the behaviour a member a
// current CLI rendered keeps for the same EC2 error: its row fails with EC2's
// error and no remedy (that CLI ensured the profile just before it uploaded,
// so the cause is propagation or a deletion since, and re-running with a
// current spawn is what the user just did), it is not retried as capacity,
// and the sweep's other members are still attempted.
func TestCLIRenderedMemberMissingProfileKeepsGoing(t *testing.T) {
	ec2Fake, client := profileHarness(t)
	state := legacyState()
	state.TotalParams = 3

	launchBatch(context.Background(), client, state, cliRenderedRows(t, 3), 3, 0)

	if got := ec2Fake.runs(); len(got) != 3 {
		t.Fatalf("RunInstances calls = %v, want one per member (3), each for its first type", got)
	}
	for _, typ := range ec2Fake.runs() {
		if typ != "t3.micro" {
			t.Errorf("RunInstances for %s: the profile error was taken for capacity", typ)
		}
	}
	if state.Failed != 3 || state.NextToLaunch != 3 || state.ErrorMessage != "" {
		t.Errorf("failed=%d next=%d sweep error=%q, want 3 failed rows and no sweep error", state.Failed, state.NextToLaunch, state.ErrorMessage)
	}
	for _, row := range state.Instances {
		if strings.Contains(row.ErrorMessage, "#1369") || !strings.Contains(row.ErrorMessage, "Invalid IAM Instance Profile name") {
			t.Errorf("row %d error = %q, want EC2's error alone", row.Index, row.ErrorMessage)
		}
	}
	if got := abortedStatus(state, completionStatus(state.Launched, state.Failed)); got != "FAILED" {
		t.Errorf("status = %s, want completionStatus's FAILED (nothing launched)", got)
	}
}

// The classification is narrow: another InvalidParameterValue, or the profile
// error for a legacy member that names its own profile, is not it.
func TestLegacyProfileClassificationIsNarrow(t *testing.T) {
	state := legacyState()
	legacy := v0146Params(false, "1h")
	profile := func(name string) *ec2.RunInstancesInput {
		return &ec2.RunInstancesInput{IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: awssdk.String(name)}}
	}
	sporedIn, ownIn := profile(spawnaws.SporedInstanceProfileName), profile("spawn-instance-abc123")
	profileErr := &fakeAPIError{code: "InvalidParameterValue", msg: missingProfileMsg}
	otherErr := &fakeAPIError{code: "InvalidParameterValue", msg: "Invalid value 'bogus.type' for instanceType"}

	var lpe *legacyProfileError
	if !errors.As(classifyLaunchError(state, legacy, sporedIn, 0, profileErr), &lpe) {
		t.Error("a legacy member refused for spored-instance-profile was not classified")
	}
	for name, err := range map[string]error{
		"other InvalidParameterValue": classifyLaunchError(state, legacy, sporedIn, 0, otherErr),
		"own profile":                 classifyLaunchError(state, legacy, ownIn, 0, profileErr),
	} {
		if errors.As(err, &lpe) {
			t.Errorf("%s: classified as a missing spored-instance-profile", name)
		}
	}
}

// fakeAPIError is a smithy.APIError.
type fakeAPIError struct{ code, msg string }

func (e *fakeAPIError) Error() string        { return "api error " + e.code + ": " + e.msg }
func (e *fakeAPIError) ErrorCode() string    { return e.code }
func (e *fakeAPIError) ErrorMessage() string { return e.msg }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault {
	return smithy.FaultClient
}
