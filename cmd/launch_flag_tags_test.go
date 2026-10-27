package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/queue"
)

// withLaunchFlagTags sets --tag and --team for one test, with the team name
// resolved by a stub instead of DynamoDB.
func withLaunchFlagTags(t *testing.T, tags []string, teamID string) {
	t.Helper()
	prevTags, prevTeam, prevResolve := launchTags, launchTeamID, resolveTeamNameFn
	prevType, prevRegion := instanceType, region
	t.Cleanup(func() {
		launchTags, launchTeamID, resolveTeamNameFn = prevTags, prevTeam, prevResolve
		instanceType, region = prevType, prevRegion
	})
	launchTags, launchTeamID = tags, teamID
	resolveTeamNameFn = func(_ context.Context, id string) (string, error) {
		if id != teamID {
			t.Errorf("team name resolved for %q, want %q", id, teamID)
		}
		return "Team One", nil
	}
	instanceType, region = "t3.micro", "us-east-1"
}

// flagTagsWant is what --tag project=p1 --tag cost-center=cc --team team-1 put
// on a single launch's instance.
var flagTagsWant = map[string]string{
	"project":         "p1",
	"cost-center":     "cc",
	"spawn:team-id":   "team-1",
	"spawn:team-name": "Team One",
}

func assertFlagTags(t *testing.T, what string, got map[string]string) {
	t.Helper()
	for k, v := range flagTagsWant {
		if got[k] != v {
			t.Errorf("%s: tag %s = %q, want %q (spawn launch --tag/--team, #1356)", what, k, got[k], v)
		}
	}
}

// sweepFile is a sweep across two regions, so the multi-region case is in it.
func sweepFile() *ParamFileFormat {
	return &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0", "ttl": "1h"},
		Params: []map[string]interface{}{
			{"alpha": 0.1, "region": "us-east-1"},
			{"alpha": 0.2, "region": "us-west-2"},
		},
	}
}

// --tag and --team reach every member of a parameter sweep, foreground and
// detached, in every region (#1356). They used to be merged in runLaunch only
// after the sweep had been handed off, so no member carried them.
func TestSweepMembersCarryTagAndTeamFlags(t *testing.T) {
	withLaunchFlagTags(t, []string{"project=p1", "cost-center=cc"}, "team-1")
	base, err := sweepBaseConfig(context.Background())
	if err != nil {
		t.Fatalf("sweepBaseConfig: %v", err)
	}

	// Foreground: each member's config as launchParameterSweep builds it, and
	// the tags Launch writes for it.
	pf := sweepFile()
	for i, row := range pf.Params {
		cfg, err := buildLaunchConfigFromParams(*base, pf.Defaults, row, "sw-1", "sw", i, len(pf.Params))
		if err != nil {
			t.Fatal(err)
		}
		defaultSweepMember(&cfg, *base, "sw", i)
		tags, err := testMemberTags(cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertFlagTags(t, "foreground member "+cfg.Region, tags)
	}

	// Detached: the tags uploaded for the orchestrator, which launches each
	// member with them (TestMemberLaunchesWithTheCLIsTags, and
	// TestMemberKeepsTheCLIsTeamAndUserTags in lambda/sweep-orchestrator).
	pf = sweepFile()
	if err := seedDetachedBootstrap(pf, *base, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
		t.Fatalf("seedDetachedBootstrap: %v", err)
	}
	for i := range pf.Params {
		assertFlagTags(t, "detached member "+pf.Params[i]["region"].(string), tagsAsMap(shippedTags(t, pf, i)))
	}
}

// And the batch-queue instance (#1356).
func TestBatchQueueInstanceCarriesTagAndTeamFlags(t *testing.T) {
	withLaunchFlagTags(t, []string{"project=p1", "cost-center=cc"}, "team-1")
	qc := &queue.QueueConfig{QueueID: "q-1", QueueName: "q", GlobalTimeout: "2h"}
	cfg, err := batchQueueLaunchConfig(context.Background(), qc)
	if err != nil {
		t.Fatalf("batchQueueLaunchConfig: %v", err)
	}
	cfg.Region, cfg.AMI = "us-east-1", "ami-0123456789abcdef0"
	tags, err := testMemberTags(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertFlagTags(t, "batch-queue instance", tags)
}

// Now that --tag reaches the batch-queue instance, a tag set over AWS's 50-tag
// limit is refused before the queue path makes any AWS call (#1336, #1356):
// before the region lookup, the queue upload, the key pair and the IAM role,
// none of which RunInstances' refusal would undo.
func TestBatchQueueOverTheTagLimitMakesNoAWSCall(t *testing.T) {
	withLaunchFlagTags(t, fiftyTags(), "")
	p := filepath.Join(t.TempDir(), "queue.json")
	body := `{"queue_id": "q-1", "queue_name": "q", "jobs": [{"job_id": "a", "command": "true", "timeout": "1m"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	prevFile, prevTmpl := batchQueueFile, queueTemplate
	t.Cleanup(func() { batchQueueFile, queueTemplate = prevFile, prevTmpl })
	batchQueueFile, queueTemplate, region = p, "", ""

	calls := 0
	t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls))
	err := launchWithBatchQueue(context.Background(), nil, nil)
	var limit *aws.TagLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("a batch queue with 50 --tag values: err = %v, want a *aws.TagLimitError", err)
	}
	if calls != 0 {
		t.Errorf("a batch queue refused for its tag count made %d AWS call(s) first", calls)
	}
}

// A --tag with the reserved spawn: prefix is refused on the sweep and batch-queue
// paths exactly as on a single launch, and by runLaunch before any of them is
// dispatched or any AWS call is made (#1356). It must not be ignored, nor reach
// a member and contend with spawn's own tag.
func TestReservedTagFlagRefusedOnEveryLaunchPath(t *testing.T) {
	const reserved = "spawn:sweep-id=mine"
	withLaunchFlagTags(t, []string{reserved}, "")
	_, single := parseKVTags(launchTags)
	if single == nil {
		t.Fatal("a single launch accepts a spawn: --tag; this test assumes it refuses one")
	}

	if _, err := sweepBaseConfig(context.Background()); err == nil || err.Error() != single.Error() {
		t.Errorf("sweep base config with --tag %s: err = %v, want the single launch's %q", reserved, err, single)
	}
	qc := &queue.QueueConfig{QueueID: "q-1", QueueName: "q", GlobalTimeout: "2h"}
	if _, err := batchQueueLaunchConfig(context.Background(), qc); err == nil || err.Error() != single.Error() {
		t.Errorf("batch-queue config with --tag %s: err = %v, want the single launch's %q", reserved, err, single)
	}

	// Through runLaunch, on the sweep and the batch-queue dispatch: refused
	// before the dispatch and before its first AWS call.
	prevQueue, prevParams, prevKeep, prevWait := batchQueueFile, params, keepOnError, waitForSSHSet
	t.Cleanup(func() {
		batchQueueFile, params, keepOnError, waitForSSHSet = prevQueue, prevParams, prevKeep, prevWait
	})
	for _, path := range []struct{ name, queueFile, params string }{
		{"batch queue", "queue.json", ""},
		{"parameter sweep", "", "alpha=1,2"},
	} {
		batchQueueFile, params = path.queueFile, path.params
		calls := 0
		t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls))
		err := runLaunch(launchCmd, nil)
		if err == nil || !strings.Contains(err.Error(), single.Error()) {
			t.Errorf("%s: runLaunch with --tag %s = %v, want the single launch's refusal %q", path.name, reserved, err, single)
		}
		if calls != 0 {
			t.Errorf("%s: a refused --tag made %d AWS call(s) first", path.name, calls)
		}
	}
}

// `spawn schedule create` has no --tag or --team, so a scheduled sweep cannot
// drop them silently: cobra refuses the flag (#1356). Whoever adds them must
// carry them into the seeded params' rendered tags (seedScheduleParams), as
// sweepBaseConfig does for `spawn launch --param-file`.
func TestScheduleCreateRefusesTagAndTeamFlags(t *testing.T) {
	for _, flag := range []string{"--tag=project=p1", "--team=team-1"} {
		err := scheduleCreateCmd.ParseFlags([]string{flag})
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("schedule create %s: err = %v, want an unknown-flag refusal rather than a dropped flag", flag, err)
		}
	}
}

// countingAWSEndpoint points the AWS SDK at a local server that counts every
// request and fails it, with static credentials and no shared config or IMDS.
func countingAWSEndpoint(t *testing.T, calls *int) string {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(home, "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, "none"))
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SECRET")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	return srv.URL
}
