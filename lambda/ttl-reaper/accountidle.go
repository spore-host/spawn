package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/spore-host/calque/pkg/calquefootprint"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"

	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/poolqueue"
	"github.com/spore-host/spawn/pkg/reaperconfig"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// The account-idle sweep (spawn#1654, part of the leave-no-trace epic
// spore-host/spore-host#752): judge, per onboarded member account and scope,
// whether anything spawn manages is active, and once nothing has been for the
// grace (24 h), plan the teardown of everything spawn created there.
//
// REPORT MODE ONLY. The sweep computes the verdict, takes the shared-account
// teardown lease (libs/lifecycle), re-checks, plans with spawn's own footprint
// rules (pkg/aws AccountFootprint, the `spawn footprint -o json` scan) and
// runs the plan through footprint.Execute in ModeReport, with a deleter
// (pkg/teardown.NewReportOnly) that cannot delete. It logs every row and a
// summary, then releases the lease. It never calls Lease.Finish and never
// deletes; arming it is spawn#1657. REAPER_ACCOUNT_IDLE=reap is accepted and
// behaves as report, saying so.
//
// # The verdict
//
// Active, in a region, when any of these is live; every read that fails counts
// as active, because a verdict built on a gap could tear down what it could
// not see:
//
//   - a spawn:managed instance pending, running, shutting-down, stopping or
//     stopped (member account, DescribeInstances);
//   - an egress proxy instance, tagged spawn:egress-proxy (member account);
//   - an FSx filesystem spawn manages that is being created or changed, or is
//     younger than the ephemeral-orphan grace (member account);
//   - a spawn-pool-* queue that has workers, messages, is younger than its
//     grace or cannot be read (member account, pkg/poolqueue's own rule);
//   - for the account, from the infra account's tables on the reaper's own
//     credentials: a sweep or a pipeline that is not finished, or a schedule
//     that will still fire. These are what the orchestrator Lambdas launch
//     from, so they cover the launches no CLI marks;
//   - calque's footprint (pkg/calquefootprint, every enabled region): any
//     resource it classes active. calque launches through spawn pools, so its
//     activity is spawn's. Read only when spawn's own signals are all idle.
//
// The global scope (IAM) is active when any scanned region is, or when a
// spawn instance is live in any region enabled in the account, scanned or
// not: a role is in use wherever its instance runs.
//
// # The lease
//
// A scope is judged only once some spawn creator has marked it active
// (lifecycle.Item.Epoch > 0; spawn's CLI calls MarkActive before it creates,
// pkg/lifecyclemark). An account whose users cannot write the lease table, or
// one spawn never ran in, has no such item: its lease is UNAVAILABLE, and it
// is never idle-torn-down, because nothing would stop a teardown racing a
// launch there. The sweep says so and skips it, and never creates the table
// itself for an account no creator has marked. A lease that cannot be read at
// all (the member role predates this grant, or DynamoDB failed) is skipped
// the same way.
//
// Each report releases the lease, which (lifecycle.Lease.Release) returns the
// scope to ACTIVE and restarts its grace: an account that stays idle is
// reported once per grace period, not on every run.
const (
	accountIdleOff    = reaperconfig.ModeOff
	accountIdleReport = reaperconfig.ModeReport
	accountIdleReap   = reaperconfig.ModeReap
)

// defaultAccountIdleGrace is how long a scope must be idle before the sweep
// plans its teardown: the epic's 24 h.
const defaultAccountIdleGrace = reaperconfig.DefaultAccountIdleGrace

// leaseControlRegion is where the lifecycle table holding scope "global"
// lives: pkg/lifecyclemark.ControlRegion, which spawn's creators write (a
// test holds the two equal).
const leaseControlRegion = "us-east-1"

// leaseOwner names the reaper in the leases it takes.
const leaseOwner = "spawn-ttl-reaper"

// egressProxyTag is pkg/egressproxy/deploy.ProxyTag (a test holds the two
// equal), repeated so the reaper does not import the proxy's launch code.
const egressProxyTag = "spawn:egress-proxy"

// The infra-account tables whose records say an orchestrator is working for a
// member account. sweepsTable and pipelinesTable are pkg/sweep's and
// pkg/pipeline's (a test holds them equal); the schedules are
// pkg/scheduler.TableName. All in pkg/scheduler.TableRegion.
const (
	sweepsTable    = "spawn-sweep-orchestration"
	pipelinesTable = "spawn-pipeline-orchestration"
)

// activeQueueReasons are pkg/poolqueue's reasons for keeping a queue that
// mean it is (or may be) in use. The others (not a pool queue, untagged, not
// spawn's, the service's) say nothing about activity.
var activeQueueReasons = map[string]bool{
	poolqueue.KeepInUse:         true,
	poolqueue.KeepCountsUnknown: true,
	poolqueue.KeepNotEmpty:      true,
	poolqueue.KeepNoCreated:     true,
	poolqueue.KeepYoung:         true,
}

// parseAccountIdle maps REAPER_ACCOUNT_IDLE onto a mode. Unset and any
// unrecognised value are report, as for every deletion sweep added after the
// reaper shipped.
func parseAccountIdle(v string) string {
	if m, ok := reaperconfig.ParseMode(v); ok {
		return m
	}
	return accountIdleReport
}

// parseAccountIdleGrace reads REAPER_ACCOUNT_IDLE_GRACE ("24h", "2d", ...).
// Unset, unparseable or not positive is the default.
func parseAccountIdleGrace(v string) time.Duration {
	if strings.TrimSpace(v) == "" {
		return defaultAccountIdleGrace
	}
	d, err := reaperconfig.ParseAccountIdleGrace(v)
	if err != nil {
		log.Printf("invalid REAPER_ACCOUNT_IDLE_GRACE %q, using default %s", v, defaultAccountIdleGrace)
		return defaultAccountIdleGrace
	}
	return d
}

// newLeaseClient is the lifecycle client for an account's lease tables, over
// cfg (the member role's credentials).
func newLeaseClient(cfg aws.Config) (*lifecycle.Client, error) {
	return lifecycle.New(lifecycle.Options{
		DynamoDB: func(region string) (lifecycle.API, error) {
			c := cfg.Copy()
			c.Region = region
			return dynamodb.NewFromConfig(c), nil
		},
		ControlRegion: leaseControlRegion,
		Owner:         leaseOwner,
	})
}

// infraTablesAPI is the one DynamoDB call the infra-account signals make, on
// the reaper's own credentials.
type infraTablesAPI interface {
	Scan(ctx context.Context, in *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// infraActivity is what the infra account's tables say is working for each
// member account. err, when set, is why they could not all be read: then every
// account counts as active.
type infraActivity struct {
	byAccount map[string][]string
	err       error
}

func (a infraActivity) reasons(account string) []string {
	if a.err != nil {
		return []string{"the infra account's sweep, pipeline and schedule tables could not all be read: " + a.err.Error()}
	}
	return a.byAccount[account]
}

// infraReader reads the infra tables at most once per run for the verdicts,
// and afresh for each post-claim re-check.
type infraReader struct {
	api    infraTablesAPI
	now    func() time.Time
	cached *infraActivity
}

func (i *infraReader) get(ctx context.Context) infraActivity {
	if i.cached == nil {
		a := i.fresh(ctx)
		i.cached = &a
	}
	return *i.cached
}

func (i *infraReader) fresh(ctx context.Context) infraActivity {
	now := time.Now
	if i.now != nil {
		now = i.now
	}
	return readInfraActivity(ctx, i.api, now())
}

// readInfraActivity scans the three tables in full. Scan, not Query: no index
// is keyed on the target account, and a record the sweep did not see would
// hide a running orchestrator.
func readInfraActivity(ctx context.Context, api infraTablesAPI, now time.Time) infraActivity {
	if api == nil {
		return infraActivity{err: errors.New("no client for the infra account's tables")}
	}
	out := infraActivity{byAccount: map[string][]string{}}
	add := func(acct, why string) {
		if acct != "" {
			out.byAccount[acct] = append(out.byAccount[acct], why)
		}
	}
	scan := func(table, projection string, each func(map[string]ddbtypes.AttributeValue)) error {
		p := dynamodb.NewScanPaginator(api, &dynamodb.ScanInput{
			TableName:                aws.String(table),
			ProjectionExpression:     aws.String(projection),
			ExpressionAttributeNames: map[string]string{"#s": "status"},
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			var nf *ddbtypes.ResourceNotFoundException
			if errors.As(err, &nf) {
				// No such table in this account (a self-hosted reaper's own
				// account normally has none): no orchestrator works from it.
				return nil
			}
			if err != nil {
				return fmt.Errorf("scan %s: %w", table, err)
			}
			for _, it := range page.Items {
				each(it)
			}
		}
		return nil
	}
	var errs []error
	if err := scan(sweepsTable, "sweep_id, aws_account_id, user_id, #s", func(it map[string]ddbtypes.AttributeValue) {
		if st := avS(it, "status"); !sweepFinished(st) {
			add(firstOf(avS(it, "aws_account_id"), avS(it, "user_id")), fmt.Sprintf("sweep %s is %s", avS(it, "sweep_id"), orUnknown(st)))
		}
	}); err != nil {
		errs = append(errs, err)
	}
	if err := scan(pipelinesTable, "pipeline_id, user_id, #s", func(it map[string]ddbtypes.AttributeValue) {
		if st := avS(it, "status"); !pipelineFinished(st) {
			add(avS(it, "user_id"), fmt.Sprintf("pipeline %s is %s", avS(it, "pipeline_id"), orUnknown(st)))
		}
	}); err != nil {
		errs = append(errs, err)
	}
	if err := scan(scheduler.TableName, "schedule_id, aws_account_id, user_id, #s, execution_count, max_executions, end_after", func(it map[string]ddbtypes.AttributeValue) {
		if schedulePending(it, now) {
			add(firstOf(avS(it, "aws_account_id"), avS(it, "user_id")), fmt.Sprintf("schedule %s will still fire", avS(it, "schedule_id")))
		}
	}); err != nil {
		errs = append(errs, err)
	}
	out.err = errors.Join(errs...)
	return out
}

// sweepFinished: pkg/sweep's terminal statuses. Anything else, including an
// unknown or missing status, is still working.
func sweepFinished(status string) bool {
	switch status {
	case "COMPLETED", "FAILED", "CANCELLED":
		return true
	}
	return false
}

// pipelineFinished: pkg/pipeline.PipelineStatus.IsTerminal's statuses.
func pipelineFinished(status string) bool { return sweepFinished(status) }

// schedulePending: an active schedule that has executions left and has not
// ended. Unparseable limits count as pending.
func schedulePending(it map[string]ddbtypes.AttributeValue, now time.Time) bool {
	if avS(it, "status") != "active" {
		return false
	}
	if maxRaw := avN(it, "max_executions"); maxRaw != "" && maxRaw != "0" {
		var maxN, count int
		if _, err := fmt.Sscan(maxRaw, &maxN); err == nil {
			if _, err := fmt.Sscan(firstOf(avN(it, "execution_count"), "0"), &count); err == nil && count >= maxN {
				return false
			}
		}
	}
	if end := avS(it, "end_after"); end != "" {
		if t, err := time.Parse(time.RFC3339Nano, end); err == nil && !t.IsZero() && t.Year() > 1 && now.After(t) {
			return false
		}
	}
	return true
}

func avS(it map[string]ddbtypes.AttributeValue, k string) string {
	if v, ok := it[k].(*ddbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func avN(it map[string]ddbtypes.AttributeValue, k string) string {
	if v, ok := it[k].(*ddbtypes.AttributeValueMemberN); ok {
		return v.Value
	}
	return ""
}

func firstOf(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "of unknown status"
	}
	return s
}

// verdict is what was active in each scope of one account: a scope with no
// reasons is idle.
type verdict struct {
	scopes map[string][]string
}

func (v verdict) add(scope, why string) {
	v.scopes[scope] = append(v.scopes[scope], why)
}

func (v verdict) reasons(scope string) []string { return v.scopes[scope] }

func (v verdict) anyActive() bool {
	for _, rs := range v.scopes {
		if len(rs) > 0 {
			return true
		}
	}
	return false
}

// liveInstanceStates are the states in which an instance holds what it uses:
// pending through stopped.
var liveInstanceStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

// accountVerdict reads every activity signal of acct, for each scanned region
// and the global scope.
func (r *reaper) accountVerdict(ctx context.Context, acct account, infra infraActivity, now time.Time) verdict {
	v := verdict{scopes: map[string][]string{lifecycle.ScopeGlobal: nil}}
	for _, region := range r.regions {
		v.scopes[region] = nil
	}
	everywhere := func(why string) {
		for scope := range v.scopes {
			v.add(scope, why)
		}
	}
	for _, why := range infra.reasons(acct.accountID) {
		everywhere("infra: " + why)
	}

	// Pool workers, once for the account, over every enabled region.
	var inUse poolqueue.InUse
	var inUseErr error
	var enabled []string
	if acct.regionsFor == nil {
		inUseErr = errors.New("no way to list the account's enabled regions")
	} else if enabled, inUseErr = instancerole.EnabledRegions(ctx, acct.regionsFor(), r.regions...); inUseErr == nil {
		regs := make(map[string]instancerole.EC2Describer, len(enabled))
		for _, region := range enabled {
			regs[region] = acct.ec2For(region)
		}
		inUse, inUseErr = poolqueue.WorkersInUse(ctx, regs)
	}
	if inUseErr != nil {
		everywhere("pool workers could not be read: " + inUseErr.Error())
	}

	scanned := map[string]bool{}
	for _, region := range r.regions {
		scanned[region] = true
		for _, why := range r.regionSignals(ctx, acct, region, inUse, inUseErr == nil, now) {
			v.add(region, why)
			v.add(lifecycle.ScopeGlobal, region+": "+why)
		}
	}
	// The global scope also sees instances in regions the reaper does not
	// scan: IAM is used wherever an instance runs. (A failed region listing
	// already made every scope active, above.)
	for _, region := range enabled {
		if scanned[region] {
			continue
		}
		if why := liveInstances(ctx, acct.ec2For(region)); why != "" {
			v.add(lifecycle.ScopeGlobal, region+": "+why)
		}
	}

	// calque, only when spawn's own signals say idle: it cannot change an
	// active verdict, and its inventory lists every enabled region.
	if !v.anyActive() {
		if why := r.calqueActivity(ctx, acct, now); why != "" {
			everywhere("calque: " + why)
		}
	}
	return v
}

// regionSignals are the member-account signals of one region.
func (r *reaper) regionSignals(ctx context.Context, acct account, region string, inUse poolqueue.InUse, inUseKnown bool, now time.Time) []string {
	var out []string
	if why := liveInstances(ctx, acct.ec2For(region)); why != "" {
		out = append(out, why)
	}
	if why := liveEgressProxies(ctx, acct.ec2For(region)); why != "" {
		out = append(out, why)
	}
	if acct.fsxFor == nil {
		out = append(out, "no FSx client, so filesystems could not be read")
	} else if why := fsxInUse(ctx, acct.fsxFor(region), now); why != "" {
		out = append(out, why)
	}
	switch {
	case acct.sqsFor == nil:
		out = append(out, "no SQS client, so pool queues could not be read")
	case inUseKnown:
		res, err := poolqueue.Sweep(ctx, acct.sqsFor(region), nil, inUse, poolqueue.Options{
			Account: acct.accountID, Region: region, Now: now, Delete: false,
		})
		if err != nil {
			out = append(out, "pool queues could not be read: "+err.Error())
			break
		}
		for _, d := range res.Queues {
			if !d.Delete && (activeQueueReasons[d.Reason] || d.Err != nil) {
				out = append(out, fmt.Sprintf("pool queue %s: %s", d.Name, d.Reason))
			}
		}
	}
	return out
}

// liveInstances is why spawn's instances make the region active, or "".
func liveInstances(ctx context.Context, api ec2API) string {
	n, err := countInstances(ctx, api, []ec2types.Filter{
		{Name: aws.String(tagprefix.FilterTag("managed")), Values: []string{"true"}},
		{Name: aws.String("instance-state-name"), Values: liveInstanceStates},
	})
	switch {
	case err != nil:
		return "instances could not be read: " + err.Error()
	case n > 0:
		return fmt.Sprintf("%d spawn instance(s) pending through stopped", n)
	}
	return ""
}

// liveEgressProxies is why the region's egress proxies make it active, or "".
func liveEgressProxies(ctx context.Context, api ec2API) string {
	n, err := countInstances(ctx, api, []ec2types.Filter{
		{Name: aws.String("tag-key"), Values: []string{egressProxyTag}},
		{Name: aws.String("instance-state-name"), Values: liveInstanceStates},
	})
	switch {
	case err != nil:
		return "egress proxies could not be read: " + err.Error()
	case n > 0:
		return fmt.Sprintf("%d egress proxy instance(s)", n)
	}
	return ""
}

func countInstances(ctx context.Context, api ec2API, filters []ec2types.Filter) (int, error) {
	if api == nil {
		return 0, errors.New("no EC2 client")
	}
	n := 0
	p := ec2.NewDescribeInstancesPaginator(api, &ec2.DescribeInstancesInput{Filters: filters})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, rv := range page.Reservations {
			n += len(rv.Instances)
		}
	}
	return n, nil
}

// fsxInUse is why spawn's FSx filesystems make the region active, or "": one
// being created or changed, or younger than ephemeralOrphanGrace (made for a
// launch that has not tagged its instance yet).
func fsxInUse(ctx context.Context, api fsxAPI, now time.Time) string {
	p := fsx.NewDescribeFileSystemsPaginator(api, &fsx.DescribeFileSystemsInput{})
	var why []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return "FSx filesystems could not be read: " + err.Error()
		}
		for _, f := range page.FileSystems {
			managed := false
			for _, t := range f.Tags {
				if aws.ToString(t.Key) == tagprefix.Tag("managed") && aws.ToString(t.Value) == "true" {
					managed = true
				}
			}
			if !managed {
				continue
			}
			id := aws.ToString(f.FileSystemId)
			switch f.Lifecycle {
			case fsxtypes.FileSystemLifecycleCreating, fsxtypes.FileSystemLifecycleUpdating:
				why = append(why, fmt.Sprintf("FSx %s is %s", id, f.Lifecycle))
			case fsxtypes.FileSystemLifecycleDeleting, fsxtypes.FileSystemLifecycleFailed:
			default:
				if c := aws.ToTime(f.CreationTime); !c.IsZero() && now.Sub(c) < ephemeralOrphanGrace {
					why = append(why, fmt.Sprintf("FSx %s was created %s ago", id, now.Sub(c).Round(time.Minute)))
				}
			}
		}
	}
	return strings.Join(why, "; ")
}

// calqueActivity is why calque's footprint makes the account active, or "".
// A listing calque could not complete is classed active by calquefootprint
// itself, so it counts here too.
func (r *reaper) calqueActivity(ctx context.Context, acct account, now time.Time) string {
	if acct.calqueFor == nil {
		return ""
	}
	fp, err := acct.calqueFor(ctx, now)
	if err != nil {
		return "its footprint could not be read: " + err.Error()
	}
	if fp == nil || fp.Report == nil {
		return "its footprint came back empty-handed"
	}
	var why []string
	for _, res := range fp.Report.Resources {
		if res.Class == footprint.ClassActive {
			why = append(why, fmt.Sprintf("%s %s is active", res.Type, res.Name))
		}
	}
	if len(fp.Incomplete) > 0 {
		why = append(why, "listings failed: "+strings.Join(fp.Incomplete, ", "))
	}
	sort.Strings(why)
	return strings.Join(why, "; ")
}

// sweepAccountIdle runs the account-idle sweep for one account.
func (r *reaper) sweepAccountIdle(ctx context.Context, acct account, infra *infraReader, now time.Time, sum *Summary) {
	// Unset is off here: configure always sets a mode (report by default),
	// so only a reaper built without one, in a test, has none.
	if r.accountIdle == accountIdleOff || r.accountIdle == "" {
		return
	}
	// Never in the infra account, nor one the reaper could not identify
	// (#750, #1660): first, before any read.
	if err := GuardTeardown(acct.accountID); err != nil {
		log.Printf("account-idle: account=%s: refused: %v", acct.label, err)
		sum.AccountIdleRefused++
		return
	}
	if acct.lease == nil {
		log.Printf("account-idle: account=%s: lease unavailable (no lifecycle client); skipped", acct.accountID)
		sum.AccountIdleLeaseUnavailable++
		return
	}
	grace := r.accountIdleGrace
	if grace <= 0 {
		grace = defaultAccountIdleGrace
	}
	v := r.accountVerdict(ctx, acct, infra.get(ctx), now)
	scopes := append(append([]string{}, r.regions...), lifecycle.ScopeGlobal)
	var report *footprint.Report
	var deleter footprint.Deleter
	for _, scope := range scopes {
		reasons := v.reasons(scope)
		active := len(reasons) > 0
		it, exists, err := acct.lease.Get(ctx, fs.Tool, scope)
		switch {
		case err != nil:
			log.Printf("account-idle: account=%s region=%s: lease unavailable (%s): %v; skipped", acct.accountID, scope, classifyScanError(err), err)
			sum.AccountIdleLeaseUnavailable++
			continue
		case !exists || it.Epoch == 0:
			log.Printf("account-idle: account=%s region=%s: lease unavailable: no spawn creator has marked this scope active "+
				"(spawn has not run here, or its users cannot write %s); never idle-torn-down; skipped", acct.accountID, scope, lifecycle.TableName)
			sum.AccountIdleLeaseUnavailable++
			continue
		}
		sum.AccountIdleScopes++
		_, idleFor, err := acct.lease.Observe(ctx, fs.Tool, scope, active)
		if err != nil {
			log.Printf("account-idle: account=%s region=%s: lease unavailable, observe failed (%s): %v; skipped", acct.accountID, scope, classifyScanError(err), err)
			sum.AccountIdleLeaseUnavailable++
			continue
		}
		if active {
			sum.AccountIdleActive++
			log.Printf("account-idle: account=%s region=%s: active: %s", acct.accountID, scope, strings.Join(reasons, "; "))
			continue
		}
		if idleFor < grace {
			sum.AccountIdleWaiting++
			log.Printf("account-idle: account=%s region=%s: idle for %s of the %s grace", acct.accountID, scope, idleFor.Round(time.Minute), grace)
			continue
		}
		lease, err := acct.lease.Claim(ctx, fs.Tool, scope, grace)
		if err != nil {
			log.Printf("account-idle: account=%s region=%s: not claimed: %v", acct.accountID, scope, err)
			continue
		}
		sum.AccountIdleClaimed++
		// Re-read every signal now that the lease stops new creators: activity
		// that started since the verdict above releases the claim.
		recheck := r.accountVerdict(ctx, acct, infra.fresh(ctx), now)
		if again := recheck.reasons(scope); len(again) > 0 {
			sum.AccountIdleReleased++
			log.Printf("account-idle: account=%s region=%s: active again after the claim (%s); released", acct.accountID, scope, strings.Join(again, "; "))
			releaseLease(ctx, lease, acct.accountID, scope)
			continue
		}
		if report == nil && acct.footprintFor != nil {
			report, deleter = acct.footprintFor(ctx, r.regions, r.roleReferences(ctx), now)
		}
		r.reportScope(ctx, acct, scope, report, deleter, lease, sum)
		if scope == lifecycle.ScopeGlobal {
			r.reportCalque(ctx, acct, now)
		}
		releaseLease(ctx, lease, acct.accountID, scope)
	}
}

// roleReferences are the schedules that name per-task roles, for the plan's
// re-checks. When they cannot be read the plan keeps every per-task role, as
// `spawn teardown` does.
func (r *reaper) roleReferences(ctx context.Context) instancerole.References {
	refs, err := instancerole.CollectReferences(ctx, r.roleRefs...)
	if err != nil {
		log.Printf("account-idle: the schedules that may name spawn-instance-* roles could not be read, so the report keeps every one: %v", err)
		return instancerole.References{}
	}
	return refs
}

func releaseLease(ctx context.Context, lease *lifecycle.Lease, account, scope string) {
	if err := lease.Release(ctx); err != nil {
		log.Printf("account-idle: account=%s region=%s: release the lease: %v", account, scope, err)
	}
}

// reportScope runs the plan for scope in report mode, under the real lease,
// and logs every row and the summary line.
func (r *reaper) reportScope(ctx context.Context, acct account, scope string, rep *footprint.Report, deleter footprint.Deleter, lease *lifecycle.Lease, sum *Summary) {
	if rep == nil || deleter == nil {
		log.Printf("account-idle: account=%s region=%s: idle past the grace, but no footprint could be planned", acct.accountID, scope)
		return
	}
	if rep.Account != acct.accountID {
		// The plan is for one account, and the deleter refuses any other.
		log.Printf("account-idle: account=%s region=%s: the footprint is for account %q; not reported", acct.accountID, scope, rep.Account)
		return
	}
	for _, w := range rep.Warnings {
		log.Printf("account-idle REPORT account=%s region=%s warning=%q", acct.accountID, scope, w)
	}
	var rs []footprint.Resource
	for _, res := range rep.Resources {
		if res.Region == scope || (scope == lifecycle.ScopeGlobal && res.Region == fs.RegionGlobal) {
			rs = append(rs, res)
		}
	}
	plan, err := footprint.NewPlan(fs.Tool, fs.Patterns, rs)
	if err != nil {
		log.Printf("account-idle: account=%s region=%s: plan: %v", acct.accountID, scope, err)
		return
	}
	// Report mode, always: reap is spawn#1657.
	res, err := footprint.Execute(ctx, plan, footprint.ModeReport, deleter, lease)
	if res == nil {
		log.Printf("account-idle: account=%s region=%s: execute: %v", acct.accountID, scope, err)
		return
	}
	for _, a := range res.Actions {
		why := a.Reason
		if a.Err != nil {
			why = a.Err.Error()
		}
		log.Printf("account-idle REPORT account=%s region=%s outcome=%s type=%s name=%s arn=%s reason=%q",
			acct.accountID, scope, a.Outcome, a.Type, a.Name, a.ARN, why)
	}
	would := res.Count(footprint.OutcomeWouldDelete)
	kept := len(res.Actions) - would
	log.Printf("account-idle REPORT account=%s region=%s would_delete=%d kept=%d", acct.accountID, scope, would, kept)
	if errors.Is(err, lifecycle.ErrLeaseLost) {
		log.Printf("account-idle: account=%s region=%s: the lease was lost mid-report (a creator marked the scope active)", acct.accountID, scope)
	}
	sum.AccountIdleReported++
	sum.AccountIdleWouldDelete += would
	sum.AccountIdleKept += kept
}

// reportCalque logs calque's footprint verdicts for the account, report only:
// the reaper is calque's backstop through calquefootprint, and reaping it is
// calque#229.
func (r *reaper) reportCalque(ctx context.Context, acct account, now time.Time) {
	if acct.calqueFor == nil {
		return
	}
	fp, err := acct.calqueFor(ctx, now)
	if err != nil || fp == nil || fp.Report == nil {
		log.Printf("account-idle: account=%s tool=calque: footprint not read: %v", acct.accountID, err)
		return
	}
	would := 0
	for _, res := range fp.Report.Resources {
		if res.Deletable {
			would++
		}
		log.Printf("account-idle REPORT tool=calque account=%s region=%s deletable=%t class=%s type=%s name=%s arn=%s reason=%q",
			acct.accountID, res.Region, res.Deletable, res.Class, res.Type, res.Name, res.ARN, res.Reason)
	}
	log.Printf("account-idle REPORT tool=calque account=%s would_delete=%d kept=%d", acct.accountID, would, len(fp.Report.Resources)-would)
}

// accountIdleModeFromEnv reads REAPER_ACCOUNT_IDLE, saying so when reap was
// asked for: reaping is spawn#1657, so reap reports.
func accountIdleModeFromEnv() string {
	m := parseAccountIdle(os.Getenv("REAPER_ACCOUNT_IDLE"))
	if m == accountIdleReap {
		log.Printf("REAPER_ACCOUNT_IDLE=reap: the account-idle sweep cannot delete yet (spawn#1657); it reports")
	}
	return m
}

// calqueFootprintFor inventories calque's resources over cfg, in every region
// enabled in the account. No buckets are passed (calque records none), so its
// S3 objects are left to `calque teardown --bucket`.
func calqueFootprintFor(clients calquefootprint.Clients, regionsAPI calquefootprint.RegionsAPI, account string) func(ctx context.Context, now time.Time) (*calquefootprint.Footprint, error) {
	return func(ctx context.Context, now time.Time) (*calquefootprint.Footprint, error) {
		regions, err := calquefootprint.EnabledRegions(ctx, regionsAPI)
		if err != nil {
			return nil, err
		}
		return calquefootprint.Inventory(ctx, clients, calquefootprint.Options{Account: account, Regions: regions, AllRegions: true}, now)
	}
}
