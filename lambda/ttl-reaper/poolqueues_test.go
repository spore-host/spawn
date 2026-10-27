package main

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/spawn/pkg/poolqueue"
	"github.com/spore-host/spawn/pkg/poolqueue/poolqueuetest"
)

// scenarioEC2 answers the pool sweep's state-only DescribeInstances from the
// shared scenario, and the instance scan (filtered on spawn tags) with
// nothing, so only the pool sweep sees the scenario's workers.
type scenarioEC2 struct {
	emptyEC2
	region *poolqueuetest.EC2
	err    error
	calls  int
}

func (f *scenarioEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, o ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	for _, flt := range in.Filters {
		if strings.HasPrefix(aws.ToString(flt.Name), "tag:") {
			return &ec2.DescribeInstancesOutput{}, nil
		}
	}
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.region.DescribeInstances(ctx, in, o...)
}

// countingSQS records every call, so "off" can be shown to make none.
type countingSQS struct {
	sqsAPI
	calls   int
	listErr error
}

func (c *countingSQS) ListQueues(ctx context.Context, in *sqs.ListQueuesInput, o ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	c.calls++
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.sqsAPI.ListQueues(ctx, in, o...)
}

// poolReaper is the shared scenario as one account, scanning only
// poolqueuetest.Region; OtherRegion is enabled but outside REAPER_REGIONS and
// holds the only worker of the "busy" pool.
func poolReaper(mode string, dryRun bool) (*reaper, *poolqueuetest.Scenario, map[string]*scenarioEC2, *countingSQS) {
	s := poolqueuetest.New()
	ec2s := map[string]*scenarioEC2{}
	for r, c := range s.EC2 {
		ec2s[r] = &scenarioEC2{region: c}
	}
	q := &countingSQS{sqsAPI: s.SQS}
	regs := &fakeRegions{regions: s.Regions()}
	r := &reaper{
		regions:       []string{poolqueuetest.Region},
		maxAge:        defaultMaxAge,
		dryRun:        dryRun,
		netResources:  netResourcesOff,
		spotRequests:  spotRequestsOff,
		elasticIPs:    elasticIPsOff,
		instanceRoles: instanceRolesOff,
		emptyBuckets:  emptyBucketsOff,
		poolQueues:    mode,
		accounts: []account{{
			label: "acct", accountID: poolqueuetest.Account,
			ec2For:   func(region string) ec2API { return ec2s[region] },
			sqsFor:   func(string) sqsAPI { return q },
			specsFor: func(string) poolSpecsAPI { return s.S3 },
		}},
	}
	r.accounts[0].regionsFor = func() regionsAPI { return regs }
	return r, s, ec2s, q
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestPoolQueueSweepAgreesWithTheCLI: reap, through the reaper's own run,
// deletes exactly what the shared scenario says, which is also what
// cmd's TestCleanupPoolQueues holds `spawn cleanup --pool-queues` to. The
// queue whose only worker is in an enabled region outside REAPER_REGIONS,
// the one holding a message, the young one, the untagged one and the one
// tagged for another run all survive.
func TestPoolQueueSweepAgreesWithTheCLI(t *testing.T) {
	r, s, ec2s, _ := poolReaper(poolQueuesReap, false)
	var sum Summary
	out := captureLogs(func() { sum, _ = r.run(context.Background()) })
	if got := sortedCopy(s.SQS.Deleted); strings.Join(got, ",") != strings.Join(poolqueuetest.WantDeletedQueues, ",") {
		t.Errorf("reap deleted queues %v, want %v\n%s", got, poolqueuetest.WantDeletedQueues, out)
	}
	if got := s.DeletedSpecRuns(); strings.Join(got, ",") != strings.Join(poolqueuetest.WantDeletedSpecRuns, ",") {
		t.Errorf("reap deleted the specs of %v, want %v\n%s", got, poolqueuetest.WantDeletedSpecRuns, out)
	}
	if ec2s[poolqueuetest.OtherRegion].calls == 0 {
		t.Error("never looked for workers in an enabled region outside REAPER_REGIONS")
	}
	if sum.PoolQueuesScanned != 6 || sum.PoolQueuesDeleted != 1 || sum.PoolQueuesWouldDelete != 0 || sum.PoolQueuesSkipped != 5 {
		t.Errorf("summary scanned/deleted/would/skipped = %d/%d/%d/%d, want 6/1/0/5",
			sum.PoolQueuesScanned, sum.PoolQueuesDeleted, sum.PoolQueuesWouldDelete, sum.PoolQueuesSkipped)
	}
	if sum.PoolSpecsDeleted != 2 {
		t.Errorf("pool_specs_deleted = %d, want 2", sum.PoolSpecsDeleted)
	}
	if !strings.Contains(out, "DELETED pool queue "+poolqueue.QueueName("abandoned")) {
		t.Errorf("no DELETED line:\n%s", out)
	}
	if len(s.S3.BadOwner) != 0 {
		t.Errorf("S3 calls without the account as expected owner: %v", s.S3.BadOwner)
	}
}

// report (the default) and a dry run over reap change nothing and log
// exactly what reap deletes; off makes no SQS call at all.
func TestPoolQueueSweepModes(t *testing.T) {
	for _, c := range []struct {
		name   string
		mode   string
		dryRun bool
	}{{"report", poolQueuesReport, false}, {"dry run beats reap", poolQueuesReap, true}} {
		t.Run(c.name, func(t *testing.T) {
			r, s, _, _ := poolReaper(c.mode, c.dryRun)
			var sum Summary
			out := captureLogs(func() { sum, _ = r.run(context.Background()) })
			if len(s.SQS.Deleted) != 0 || len(s.DeletedSpecRuns()) != 0 {
				t.Errorf("deleted queues %v, specs %v", s.SQS.Deleted, s.DeletedSpecRuns())
			}
			if sum.PoolQueuesWouldDelete != 1 || sum.PoolQueuesDeleted != 0 || sum.PoolSpecsWouldDelete != 2 {
				t.Errorf("would/deleted/specs-would = %d/%d/%d, want 1/0/2", sum.PoolQueuesWouldDelete, sum.PoolQueuesDeleted, sum.PoolSpecsWouldDelete)
			}
			would := regexp.MustCompile(`WOULD delete pool queue (\S+)`).FindAllStringSubmatch(out, -1)
			var names []string
			for _, m := range would {
				names = append(names, m[1])
			}
			if strings.Join(sortedCopy(names), ",") != strings.Join(poolqueuetest.WantDeletedQueues, ",") {
				t.Errorf("report named %v, want exactly %v:\n%s", names, poolqueuetest.WantDeletedQueues, out)
			}
		})
	}
	t.Run("off", func(t *testing.T) {
		r, _, _, q := poolReaper(poolQueuesOff, false)
		captureLogs(func() { _, _ = r.run(context.Background()) })
		if q.calls != 0 {
			t.Errorf("off still listed queues %d time(s)", q.calls)
		}
	})
}

// One enabled region that cannot be read means no queue in the account is
// even listed: a pool with workers there would look abandoned.
func TestPoolQueueSweepFailsClosedOnAnUnreadableRegion(t *testing.T) {
	r, s, ec2s, q := poolReaper(poolQueuesReap, false)
	ec2s[poolqueuetest.OtherRegion].err = errors.New("RequestLimitExceeded: slow down")
	var sum Summary
	captureLogs(func() { sum, _ = r.run(context.Background()) })
	if q.calls != 0 || len(s.SQS.Deleted) != 0 {
		t.Errorf("swept queues (%d list calls, deleted %v) without knowing which pools have workers", q.calls, s.SQS.Deleted)
	}
	if sum.Errors == 0 {
		t.Error("an unreadable region must count as an error")
	}

	r, _, _, q = poolReaper(poolQueuesReap, false)
	r.accounts[0].regionsFor = func() regionsAPI { return &fakeRegions{err: errors.New("Throttling")} }
	captureLogs(func() { sum, _ = r.run(context.Background()) })
	if q.calls != 0 || sum.Errors == 0 {
		t.Errorf("unreadable region list: %d SQS calls, %d errors; want none and an error", q.calls, sum.Errors)
	}
}

// A member role predating the #1351 grant is a standing configuration fact,
// counted once per account, not an operational error.
func TestPoolQueueSweepDeniedIsCountedApart(t *testing.T) {
	r, _, _, q := poolReaper(poolQueuesReap, false)
	q.listErr = apiErr("AccessDenied")
	var sum Summary
	captureLogs(func() { sum, _ = r.run(context.Background()) })
	if sum.PoolQueuesAccountsDenied != 1 || sum.Errors != 0 {
		t.Errorf("denied/errors = %d/%d, want 1/0", sum.PoolQueuesAccountsDenied, sum.Errors)
	}
}

// Unset is report, never reap: new deletion behaviour. A typo is report too,
// and the template default, which an upgraded stack gets, says the same.
func TestPoolQueuesDefaultIsReport(t *testing.T) {
	for v, want := range map[string]string{
		"": poolQueuesReport, "reep": poolQueuesReport, "REAP": poolQueuesReap,
		"off": poolQueuesOff, " report ": poolQueuesReport,
	} {
		if got := parsePoolQueues(v); got != want {
			t.Errorf("parsePoolQueues(%q) = %q, want %q", v, got, want)
		}
	}
	b, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^  PoolQueues:\n    Type: String\n    Default: '([a-z]*)'`).FindSubmatch(b)
	if m == nil {
		t.Fatal("template.yaml has no PoolQueues parameter with a quoted default")
	}
	if string(m[1]) != poolQueuesReport {
		t.Errorf("template default PoolQueues=%q, want report", m[1])
	}
	if !regexp.MustCompile(`(?m)^\s+REAPER_POOL_QUEUES: !Ref PoolQueues\s*$`).Match(b) {
		t.Error("template.yaml does not pass PoolQueues to the function as REAPER_POOL_QUEUES")
	}
}

// The reaper's interfaces cover everything poolqueue.Sweep calls.
var (
	_ poolqueue.SQSAPI = sqsAPI(nil)
	_ poolqueue.S3API  = poolSpecsAPI(nil)
)
