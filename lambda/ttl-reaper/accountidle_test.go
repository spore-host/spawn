package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/calque/pkg/calquefootprint"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	libtags "github.com/spore-host/libs/tags"

	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
	"github.com/spore-host/spawn/pkg/pipeline"
	spawnsweep "github.com/spore-host/spawn/pkg/sweep"
)

const idleAcct = "111122223333"

// clock is the injected time every part of a test reads.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// idleEC2 is a member account's EC2: instances by kind ("managed" for the
// spawn:managed filter, "egress" for the proxy tag), the enabled regions, and
// an error for every DescribeInstances when set.
type idleEC2 struct {
	mu      sync.Mutex
	managed int
	egress  int
	err     error
	// managedErr fails only the spawn:managed instance query, so that one
	// signal is tested alone.
	managedErr error
	regions    []string
	calls      int
}

func (e *idleEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	n := 0
	for _, f := range in.Filters {
		switch aws.ToString(f.Name) {
		case "tag:spawn:managed":
			if e.managedErr != nil {
				return nil, e.managedErr
			}
			n = e.managed
		case "tag-key":
			if len(f.Values) == 1 && f.Values[0] == egressProxyTag {
				n = e.egress
			}
		}
	}
	var insts []ec2types.Instance
	for i := 0; i < n; i++ {
		insts = append(insts, ec2types.Instance{InstanceId: aws.String("i-0123456789abcdef0"),
			State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}})
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: insts}}}, nil
}

func (e *idleEC2) TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	panic("the account-idle sweep terminated an instance")
}

func (e *idleEC2) CancelSpotInstanceRequests(context.Context, *ec2.CancelSpotInstanceRequestsInput, ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	panic("the account-idle sweep cancelled a Spot request")
}

func (e *idleEC2) DescribeRegions(context.Context, *ec2.DescribeRegionsInput, ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	var out []ec2types.Region
	for _, r := range e.regions {
		out = append(out, ec2types.Region{RegionName: aws.String(r)})
	}
	return &ec2.DescribeRegionsOutput{Regions: out}, nil
}

type idleFSx struct {
	fss []fsxtypes.FileSystem
	err error
}

func (f *idleFSx) DescribeFileSystems(context.Context, *fsx.DescribeFileSystemsInput, ...func(*fsx.Options)) (*fsx.DescribeFileSystemsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &fsx.DescribeFileSystemsOutput{FileSystems: f.fss}, nil
}

func (f *idleFSx) DeleteFileSystem(context.Context, *fsx.DeleteFileSystemInput, ...func(*fsx.Options)) (*fsx.DeleteFileSystemOutput, error) {
	panic("the account-idle sweep deleted a filesystem")
}

// idleSQS has no pool queue unless err is set.
type idleSQS struct{ err error }

func (q *idleSQS) ListQueues(context.Context, *sqs.ListQueuesInput, ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	if q.err != nil {
		return nil, q.err
	}
	return &sqs.ListQueuesOutput{}, nil
}
func (q *idleSQS) ListQueueTags(context.Context, *sqs.ListQueueTagsInput, ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error) {
	return &sqs.ListQueueTagsOutput{}, nil
}
func (q *idleSQS) GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	return &sqs.GetQueueAttributesOutput{}, nil
}
func (q *idleSQS) DeleteQueue(context.Context, *sqs.DeleteQueueInput, ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	panic("the account-idle sweep deleted a queue")
}

// infraTables answers Scan by table name.
type infraTables struct {
	items map[string][]map[string]ddbtypes.AttributeValue
	err   map[string]error
	scans []string
}

func (f *infraTables) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	name := aws.ToString(in.TableName)
	f.scans = append(f.scans, name)
	if err := f.err[name]; err != nil {
		return nil, err
	}
	return &dynamodb.ScanOutput{Items: f.items[name]}, nil
}

func s(v string) ddbtypes.AttributeValue { return &ddbtypes.AttributeValueMemberS{Value: v} }

// recordingDeleter re-reads every resource as the plan has it and records
// any Delete, which report mode must never make.
type recordingDeleter struct {
	mu        sync.Mutex
	described int
	deleted   []string
}

func (d *recordingDeleter) Describe(_ context.Context, r footprint.Resource) (footprint.Resource, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.described++
	return footprint.Resource{Tags: r.Tags}, nil
}

func (d *recordingDeleter) Delete(_ context.Context, r footprint.Resource) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted = append(d.deleted, r.ARN)
	return nil
}

// spawnRes is a resource the footprint would report, finished by its rules.
func spawnRes(arn, typ, region, name string) footprint.Resource {
	r := footprint.Resource{ARN: arn, Type: typ, Region: region, Name: name,
		Tags: map[string]string{libtags.ManagedBy: "spawn", "spawn:managed": "true"}}
	fs.Finish(&r, fs.Facts{Account: idleAcct})
	return r
}

// idleWorld is one member account and the reaper around it.
type idleWorld struct {
	t       *testing.T
	clock   *clock
	table   *lifecycletest.Fake
	lease   *lifecycle.Client
	ec2     *idleEC2
	fsx     *idleFSx
	sqs     *idleSQS
	infra   *infraTables
	deleter *recordingDeleter
	report  *footprint.Report
	calque  *calquefootprint.Footprint
	calqErr error
	r       *reaper
	acct    account
	logs    *bytes.Buffer
}

func newIdleWorld(t *testing.T) *idleWorld {
	t.Helper()
	w := &idleWorld{t: t, clock: &clock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)},
		table: lifecycletest.New(), ec2: &idleEC2{regions: []string{"us-east-1"}}, fsx: &idleFSx{}, sqs: &idleSQS{},
		infra: &infraTables{items: map[string][]map[string]ddbtypes.AttributeValue{}, err: map[string]error{}}, deleter: &recordingDeleter{}}
	lc, err := lifecycle.New(lifecycle.Options{
		DynamoDB:      func(string) (lifecycle.API, error) { return w.table, nil },
		ControlRegion: leaseControlRegion, Owner: leaseOwner, Now: w.clock.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	w.lease = lc
	w.report = footprint.NewReport(fs.Tool, idleAcct, w.clock.Now())
	w.report.Resources = []footprint.Resource{
		spawnRes("arn:aws:ec2:us-east-1:"+idleAcct+":security-group/sg-0123456789abcdef0", fs.TypeSecurityGroup, "us-east-1", "spawn-mpi-x"),
		spawnRes("arn:aws:iam::"+idleAcct+":role/spawn-instance-abc", fs.TypeRole, fs.RegionGlobal, "spawn-instance-abc"),
		// Not deletable: no pattern for its type.
		spawnRes("arn:aws:logs:us-east-1:"+idleAcct+":log-group:/aws/lambda/spawn-x", fs.TypeLogGroup, "us-east-1", "/aws/lambda/spawn-x"),
	}
	w.r = &reaper{regions: []string{"us-east-1"}, accountIdle: accountIdleReport, accountIdleGrace: 24 * time.Hour}
	w.acct = account{
		label: idleAcct, accountID: idleAcct,
		ec2For:     func(string) ec2API { return w.ec2 },
		fsxFor:     func(string) fsxAPI { return w.fsx },
		sqsFor:     func(string) sqsAPI { return w.sqs },
		regionsFor: func() regionsAPI { return w.ec2 },
		lease:      w.lease,
		footprintFor: func(context.Context, []string, instancerole.References, time.Time) (*footprint.Report, footprint.Deleter) {
			return w.report, w.deleter
		},
		calqueFor: func(context.Context, time.Time) (*calquefootprint.Footprint, error) {
			if w.calqErr != nil {
				return nil, w.calqErr
			}
			if w.calque == nil {
				return &calquefootprint.Footprint{Report: footprint.NewReport(calquefootprint.Tool, idleAcct, w.clock.Now())}, nil
			}
			return w.calque, nil
		},
	}
	w.logs = &bytes.Buffer{}
	log.SetOutput(w.logs)
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	return w
}

// markActive is a spawn creator marking scope, as pkg/lifecyclemark does.
func (w *idleWorld) markActive(scopes ...string) {
	w.t.Helper()
	for _, sc := range scopes {
		if err := w.lease.MarkActive(context.Background(), fs.Tool, sc); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *idleWorld) run() Summary {
	w.t.Helper()
	var sum Summary
	w.r.sweepAccountIdle(context.Background(), w.acct, &infraReader{api: w.infra, now: w.clock.Now}, w.clock.Now(), &sum)
	return sum
}

func (w *idleWorld) item(scope string) lifecycle.Item {
	w.t.Helper()
	it, _, err := w.lease.Get(context.Background(), fs.Tool, scope)
	if err != nil {
		w.t.Fatal(err)
	}
	return it
}

// The epic's acceptance path: an idle account is observed, waits out the
// grace, is claimed, re-checked, reported with zero deletes, and released.
func TestAccountIdleReportsAfterTheGraceAndDeletesNothing(t *testing.T) {
	w := newIdleWorld(t)
	w.markActive("us-east-1", lifecycle.ScopeGlobal)

	sum := w.run()
	if sum.AccountIdleWaiting != 2 || sum.AccountIdleClaimed != 0 {
		t.Fatalf("first run: %+v, want both scopes idle and waiting", sum)
	}
	if it := w.item("us-east-1"); it.State != lifecycle.StateIdle || it.IdleSince.IsZero() {
		t.Fatalf("after an idle observation the item is %+v, want IDLE with idleSince", it)
	}

	w.clock.add(23 * time.Hour)
	if sum := w.run(); sum.AccountIdleClaimed != 0 {
		t.Fatalf("23h in: claimed %d, want none within the 24h grace", sum.AccountIdleClaimed)
	}

	w.clock.add(2 * time.Hour)
	sum = w.run()
	if sum.AccountIdleClaimed != 2 || sum.AccountIdleReported != 2 {
		t.Fatalf("25h in: %+v, want both scopes claimed and reported", sum)
	}
	if len(w.deleter.deleted) != 0 {
		t.Fatalf("report mode deleted %v", w.deleter.deleted)
	}
	if w.deleter.described == 0 {
		t.Fatal("report mode re-read nothing: the plan was not run through Execute")
	}
	if sum.AccountIdleWouldDelete != 2 {
		t.Errorf("would delete %d, want the security group and the role", sum.AccountIdleWouldDelete)
	}
	logs := w.logs.String()
	for _, want := range []string{
		"account-idle REPORT account=" + idleAcct + " region=us-east-1 would_delete=1 kept=1",
		"account-idle REPORT account=" + idleAcct + " region=global would_delete=1 kept=0",
		"outcome=would-delete type=AWS::EC2::SecurityGroup name=spawn-mpi-x",
		"outcome=kept type=AWS::Logs::LogGroup",
		"outcome=would-delete type=AWS::IAM::Role name=spawn-instance-abc",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	// Released, never finished: the items stay, ACTIVE, with no lease.
	for _, sc := range []string{"us-east-1", lifecycle.ScopeGlobal} {
		if it := w.item(sc); it.State != lifecycle.StateActive || it.Lease != nil {
			t.Errorf("%s after the report: %+v, want ACTIVE with no lease (released, not finished)", sc, it)
		}
	}
}

// Rule 1 of CLAUDE.md's gates, for the report-mode invariant: run the same
// plan through footprint.Execute in reap mode with the same deleter, and the
// recording deleter sees deletes; so the zero above is report mode's doing.
func TestAccountIdleReportModeIsWhatKeepsTheDeleterIdle(t *testing.T) {
	w := newIdleWorld(t)
	plan, err := footprint.NewPlan(fs.Tool, fs.Patterns, w.report.Resources)
	if err != nil {
		t.Fatal(err)
	}
	w.markActive("x")
	if _, _, err := w.lease.Observe(context.Background(), fs.Tool, "x", false); err != nil {
		t.Fatal(err)
	}
	l, err := w.lease.Claim(context.Background(), fs.Tool, "x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := footprint.Execute(context.Background(), plan, footprint.ModeReap, w.deleter, l); err != nil {
		t.Fatal(err)
	}
	if len(w.deleter.deleted) == 0 {
		t.Fatal("reap mode through the same deleter deleted nothing: the report-mode test proves nothing")
	}
}

// Every activity signal makes the scope active, and a read that fails counts
// as active.
func TestAccountIdleEverySignalIsActivity(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(w *idleWorld){
		"a spawn instance":      func(w *idleWorld) { w.ec2.managed = 1 },
		"an egress proxy":       func(w *idleWorld) { w.ec2.egress = 1 },
		"instances unreadable":  func(w *idleWorld) { w.ec2.managedErr = boom },
		"all of EC2 unreadable": func(w *idleWorld) { w.ec2.err = boom },
		"an FSx being created": func(w *idleWorld) {
			w.fsx.fss = []fsxtypes.FileSystem{{FileSystemId: aws.String("fs-1"), Lifecycle: fsxtypes.FileSystemLifecycleCreating,
				Tags: []fsxtypes.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}}}}
		},
		"a young FSx": func(w *idleWorld) {
			w.fsx.fss = []fsxtypes.FileSystem{{FileSystemId: aws.String("fs-1"), Lifecycle: fsxtypes.FileSystemLifecycleAvailable,
				CreationTime: aws.Time(w.clock.Now().Add(-5 * time.Minute)),
				Tags:         []fsxtypes.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}}}}
		},
		"FSx unreadable":   func(w *idleWorld) { w.fsx.err = boom },
		"pools unreadable": func(w *idleWorld) { w.sqs.err = boom },
		"a running sweep":  func(w *idleWorld) { w.infra.items[sweepsTable] = sweepItem("RUNNING") },
		"a sweep, untargeted": func(w *idleWorld) {
			w.infra.items[sweepsTable] = []map[string]ddbtypes.AttributeValue{{"sweep_id": s("sw"), "user_id": s(idleAcct), "status": s("INITIALIZING")}}
		},
		"a running pipeline": func(w *idleWorld) {
			w.infra.items[pipelinesTable] = []map[string]ddbtypes.AttributeValue{{"pipeline_id": s("p"), "user_id": s(idleAcct), "status": s("RUNNING")}}
		},
		"a pending schedule": func(w *idleWorld) {
			w.infra.items["spawn-schedules"] = []map[string]ddbtypes.AttributeValue{{"schedule_id": s("sc"), "aws_account_id": s(idleAcct), "status": s("active")}}
		},
		"infra unreadable": func(w *idleWorld) { w.infra.err[pipelinesTable] = boom },
		"calque active": func(w *idleWorld) {
			rep := footprint.NewReport(calquefootprint.Tool, idleAcct, w.clock.Now())
			rep.Resources = []footprint.Resource{{ARN: "arn:aws:sqs:us-east-1:" + idleAcct + ":calque-pool-x", Type: "AWS::SQS::Queue", Name: "calque-pool-x", Class: footprint.ClassActive}}
			w.calque = &calquefootprint.Footprint{Report: rep}
		},
		"calque unreadable": func(w *idleWorld) { w.calqErr = boom },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := newIdleWorld(t)
			w.markActive("us-east-1", lifecycle.ScopeGlobal)
			setup(w)
			sum := w.run()
			if sum.AccountIdleActive != 2 {
				t.Errorf("%+v: want both scopes active\n%s", sum, w.logs.String())
			}
			if it := w.item("us-east-1"); it.State != lifecycle.StateActive || !it.IdleSince.IsZero() {
				t.Errorf("observed %+v, want ACTIVE", it)
			}
		})
	}
}

func sweepItem(status string) []map[string]ddbtypes.AttributeValue {
	return []map[string]ddbtypes.AttributeValue{{"sweep_id": s("sw"), "aws_account_id": s(idleAcct), "status": s(status)}}
}

// The infra-account signals are per member account: another account's
// sweep, a finished one, and a schedule that has run out or ended are not
// this account's activity.
func TestAccountIdleInfraSignalsArePerAccount(t *testing.T) {
	w := newIdleWorld(t)
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	other := "444455556666"
	w.infra.items[sweepsTable] = append(sweepItem("COMPLETED"),
		map[string]ddbtypes.AttributeValue{"sweep_id": s("theirs"), "aws_account_id": s(other), "status": s("RUNNING")})
	w.infra.items[pipelinesTable] = []map[string]ddbtypes.AttributeValue{{"pipeline_id": s("p"), "user_id": s(other), "status": s("RUNNING")}}
	w.infra.items["spawn-schedules"] = []map[string]ddbtypes.AttributeValue{
		{"schedule_id": s("done"), "aws_account_id": s(idleAcct), "status": s("active"),
			"max_executions": &ddbtypes.AttributeValueMemberN{Value: "3"}, "execution_count": &ddbtypes.AttributeValueMemberN{Value: "3"}},
		{"schedule_id": s("ended"), "aws_account_id": s(idleAcct), "status": s("active"), "end_after": s("2026-01-01T00:00:00Z")},
		{"schedule_id": s("paused"), "aws_account_id": s(idleAcct), "status": s("paused")},
		{"schedule_id": s("theirs"), "aws_account_id": s(other), "status": s("active")},
	}
	if sum := w.run(); sum.AccountIdleActive != 0 || sum.AccountIdleWaiting != 2 {
		t.Fatalf("%+v: another account's work made this one active\n%s", sum, w.logs.String())
	}
	a := readInfraActivity(context.Background(), w.infra, w.clock.Now())
	if len(a.reasons(other)) != 3 {
		t.Errorf("the other account's reasons %v, want its sweep, pipeline and schedule", a.reasons(other))
	}
}

// A missing table is no records (a self-hosted reaper's own account has
// none); any other error is unreadable, and so active.
func TestAccountIdleMissingInfraTableIsNoRecords(t *testing.T) {
	w := newIdleWorld(t)
	for _, tb := range []string{sweepsTable, pipelinesTable, "spawn-schedules"} {
		w.infra.err[tb] = &ddbtypes.ResourceNotFoundException{Message: aws.String("no table")}
	}
	if a := readInfraActivity(context.Background(), w.infra, w.clock.Now()); a.err != nil {
		t.Fatalf("missing tables read as %v, want no records", a.err)
	}
	if a := readInfraActivity(context.Background(), nil, w.clock.Now()); len(a.reasons(idleAcct)) == 0 {
		t.Error("no infra client read as no activity, want active")
	}
}

// A creator that marks the scope between the claim and the re-check, or any
// activity found by the re-check, releases the claim without a report.
func TestAccountIdlePostClaimRecheckReleases(t *testing.T) {
	w := newIdleWorld(t)
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(25 * time.Hour)
	// Activity starts after the first verdict of the run and before the
	// re-check: the infra tables are read afresh for the re-check.
	reads := 0
	ir := &infraReader{api: &countingInfra{inner: w.infra, onScan: func() {
		reads++
		if reads == 4 { // the second read (the re-check) starts at the 4th Scan
			w.infra.items[sweepsTable] = sweepItem("RUNNING")
		}
	}}, now: w.clock.Now}
	var sum Summary
	w.r.sweepAccountIdle(context.Background(), w.acct, ir, w.clock.Now(), &sum)
	if sum.AccountIdleClaimed == 0 || sum.AccountIdleReleased == 0 {
		t.Fatalf("%+v: want a claim released by the re-check\n%s", sum, w.logs.String())
	}
	if sum.AccountIdleReported != 0 || w.deleter.described != 0 {
		t.Errorf("a released claim was reported (%d) or planned (%d re-reads)", sum.AccountIdleReported, w.deleter.described)
	}
	if !strings.Contains(w.logs.String(), "active again after the claim") {
		t.Errorf("no release line:\n%s", w.logs.String())
	}
	if it := w.item("us-east-1"); it.State != lifecycle.StateActive || it.Lease != nil {
		t.Errorf("after the release: %+v, want ACTIVE with no lease", it)
	}
}

type countingInfra struct {
	inner  *infraTables
	onScan func()
}

func (c *countingInfra) Scan(ctx context.Context, in *dynamodb.ScanInput, o ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	c.onScan()
	return c.inner.Scan(ctx, in, o...)
}

// No spawn creator has marked the scope (no item, or only observed ones): the
// lease is unavailable, and the scope is skipped without the sweep creating
// anything.
func TestAccountIdleLeaseUnavailableIsSkipped(t *testing.T) {
	w := newIdleWorld(t)
	sum := w.run()
	if sum.AccountIdleLeaseUnavailable != 2 || sum.AccountIdleScopes != 0 {
		t.Fatalf("%+v: want both scopes skipped as lease unavailable", sum)
	}
	if n := w.table.Calls("CreateTable"); n != 0 {
		t.Errorf("the sweep created the lifecycle table (%d) for an account no creator marked", n)
	}
	if !strings.Contains(w.logs.String(), "lease unavailable: no spawn creator has marked") {
		t.Errorf("no lease-unavailable line:\n%s", w.logs.String())
	}
	// An item only ever observed (epoch 0) is no better.
	if _, _, err := w.lease.Observe(context.Background(), fs.Tool, "us-east-1", false); err != nil {
		t.Fatal(err)
	}
	w.clock.add(48 * time.Hour)
	if sum := w.run(); sum.AccountIdleClaimed != 0 || sum.AccountIdleLeaseUnavailable != 2 {
		t.Errorf("an observed-only item: %+v, want skipped", sum)
	}
}

// A lease that cannot be read is skipped with a warning.
func TestAccountIdleUnreadableLeaseIsSkipped(t *testing.T) {
	w := newIdleWorld(t)
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	denied, err := lifecycle.New(lifecycle.Options{
		DynamoDB:      func(string) (lifecycle.API, error) { return nil, errors.New("AccessDeniedException: not authorized") },
		ControlRegion: leaseControlRegion, Now: w.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.acct.lease = denied
	if sum := w.run(); sum.AccountIdleLeaseUnavailable != 2 || sum.AccountIdleScopes != 0 {
		t.Fatalf("%+v: want both scopes skipped", sum)
	}
	if !strings.Contains(w.logs.String(), "lease unavailable") {
		t.Errorf("no warning:\n%s", w.logs.String())
	}
}

// The infra account, and an account the reaper could not identify, are
// refused before anything is read.
func TestAccountIdleRefusesTheInfraAccount(t *testing.T) {
	for _, id := range []string{InfraAccountID, ""} {
		w := newIdleWorld(t)
		w.acct.accountID, w.acct.label = id, "x"
		if sum := w.run(); sum.AccountIdleRefused != 1 {
			t.Errorf("account %q: %+v, want refused", id, sum)
		}
		if w.ec2.calls != 0 || len(w.infra.scans) != 0 || w.table.Calls("GetItem") != 0 {
			t.Errorf("account %q: read before the guard (ec2 %d, infra %v, lease %d)", id, w.ec2.calls, w.infra.scans, w.table.Calls("GetItem"))
		}
	}
}

// REAPER_ACCOUNT_IDLE: report by default, off, and reap, which reports until
// spawn#1657.
func TestAccountIdleModes(t *testing.T) {
	for v, want := range map[string]string{"": accountIdleReport, "report": accountIdleReport, "off": accountIdleOff, "reap": accountIdleReap, "bogus": accountIdleReport} {
		if got := parseAccountIdle(v); got != want {
			t.Errorf("parseAccountIdle(%q) = %q, want %q", v, got, want)
		}
	}
	for v, want := range map[string]time.Duration{"": 24 * time.Hour, "48h": 48 * time.Hour, "2d": 48 * time.Hour, "-1h": 24 * time.Hour, "x": 24 * time.Hour} {
		if got := parseAccountIdleGrace(v); got != want {
			t.Errorf("parseAccountIdleGrace(%q) = %s, want %s", v, got, want)
		}
	}
	w := newIdleWorld(t)
	w.r.accountIdle = accountIdleReap
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(25 * time.Hour)
	if sum := w.run(); sum.AccountIdleReported != 2 || len(w.deleter.deleted) != 0 {
		t.Errorf("reap mode: %+v, deleted %v; want a report and no delete", sum, w.deleter.deleted)
	}
	w2 := newIdleWorld(t)
	w2.r.accountIdle = accountIdleOff
	w2.markActive("us-east-1")
	if sum := w2.run(); sum.AccountIdleScopes != 0 || w2.ec2.calls != 0 {
		t.Errorf("off: %+v, %d EC2 calls; want nothing", sum, w2.ec2.calls)
	}
}

// The constants the reaper repeats to keep its imports small agree with
// their sources.
func TestAccountIdleConstantsAgree(t *testing.T) {
	if leaseControlRegion != lifecyclemark.ControlRegion {
		t.Errorf("leaseControlRegion %q, pkg/lifecyclemark.ControlRegion %q: the reaper would observe a global item no creator writes",
			leaseControlRegion, lifecyclemark.ControlRegion)
	}
	if egressProxyTag != deploy.ProxyTag {
		t.Errorf("egressProxyTag %q, deploy.ProxyTag %q", egressProxyTag, deploy.ProxyTag)
	}
	if sweepsTable != spawnsweep.DefaultResourceNames().DynamoTableName {
		t.Errorf("sweepsTable %q, pkg/sweep's %q", sweepsTable, spawnsweep.DefaultResourceNames().DynamoTableName)
	}
	if pipelinesTable != pipeline.DefaultTableName {
		t.Errorf("pipelinesTable %q, pkg/pipeline's %q", pipelinesTable, pipeline.DefaultTableName)
	}
	for _, st := range []pipeline.PipelineStatus{pipeline.StatusInitializing, pipeline.StatusRunning, pipeline.StatusCompleted, pipeline.StatusFailed, pipeline.StatusCancelled} {
		if pipelineFinished(string(st)) != st.IsTerminal() {
			t.Errorf("pipeline status %s: finished=%t, IsTerminal=%t", st, pipelineFinished(string(st)), st.IsTerminal())
		}
	}
}
