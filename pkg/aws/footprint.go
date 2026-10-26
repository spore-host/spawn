package aws

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	spawnscheduler "github.com/spore-host/spawn/pkg/scheduler"
)

// ControlPlaneResource is one long-lived thing spawn created that is NOT a
// launched instance or its direct attachments — a Lambda, its log group, its
// execution role, an artifact bucket, a schedule rule, a state table.
//
// These are what "leave no trace" does not currently cover (#653). The TTL
// reaper is the backstop for instances; nothing is the backstop for the reaper
// itself, and nothing was even ENUMERATING it.
type ControlPlaneResource struct {
	// json tags on every field: `spawn footprint -o json` is a machine contract,
	// and without them the payload was snake_case at the top level and PascalCase
	// inside it — `control_plane[].Service`. A consumer should not have to know
	// which half of the struct it is reading.
	Service string `json:"service"` // lambda, logs, s3, iam, events, scheduler, dynamodb
	Kind    string `json:"kind"`    // function, log-group, bucket, role, instance-profile, event-bus, rule, schedule, table
	Name    string `json:"name"`
	Region  string `json:"region"` // empty for global (IAM, and S3 bucket names that carry no region)
	// Detail is the one fact that matters for this kind: a log group's retention,
	// a bucket's creation date, a function's last modification.
	Detail string `json:"detail,omitempty"`
	// Created is zero when the API does not report it (log groups report a
	// creation time; S3 reports one; Lambda does not, only LastModified).
	Created time.Time `json:"created,omitempty"`
	// Why records WHICH signal matched, so a reader can tell a confident
	// identification from a guess. Three attempts at a footprint list during #653
	// were each wrong — the issue's own table, a prefix-filtered query, and a
	// "cleanup candidates" list — so the provenance of every row is printed
	// rather than assumed.
	Why string `json:"why,omitempty"`
	// Warn is a specific, evidence-backed hazard for this row. Empty when none.
	Warn string `json:"warn,omitempty"`
}

// footprintNamePrefixes are the name patterns that identify a spore.host
// resource when a TAG does not.
//
// The tag half of this (DiscoverManagedResources, spawn:managed=true) is the
// authoritative one, and this half exists because most of the control plane
// predates tagging it: every one of the 14 log groups found in #653 was
// untagged, and the operated lambdas were unstamped until #768.
//
// Matching on names is a compromise with a known failure mode, stated here so it
// is not rediscovered: it misses anything named differently. `scheduler-handler`
// has NO prefix at all (scripts/deploy-scheduler-handler.sh defaults
// SPAWN_LAMBDA_NAME to it), which is why the deploy census had to stop
// hardcoding function names. So the report always says how many rows came from
// each half, and never claims to be exhaustive.
var footprintNamePrefixes = []string{"spawn", "spore", "spored", "lagotto", "truffle"}

// footprintExactNames are spore.host resources whose names carry NO prefix, so
// the list above cannot find them. Every entry was discovered by enumerating an
// account rather than by recollection, which is the only way they could be.
//
// This exists because the prefix list failed on its first real run: the orphaned
// log group /aws/lambda/github-oauth-bridge — the exact case the orphan check
// below was written for, a log group that outlived the function it belonged to —
// was invisible to it. A blind spot that hides the motivating example is not an
// acceptable blind spot.
//
// Keep this list honest rather than long: add a name only after seeing it in an
// account, and never guess. A wrong entry claims something is spore.host's when
// it is not, which is worse than missing it.
var footprintExactNames = map[string]bool{
	"github-oauth-bridge": true, // Lambda removed; its log group outlived it
	"scheduler-handler":   true, // scripts/deploy-scheduler-handler.sh default
	"portal-phone-home":   true, // spore-portal onboarding
	"prism-bot":           true,
}

func matchesFootprintName(name string) bool {
	l := strings.ToLower(strings.TrimPrefix(name, "/aws/lambda/"))
	l = strings.TrimPrefix(l, "/aws/imagebuilder/")
	for _, p := range footprintNamePrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return footprintExactNames[l]
}

// ScanControlPlane enumerates the control-plane resources in one region.
//
// Read-only, and deliberately tolerant: a caller may lack permission on any one
// service, and a partial inventory is far more useful than an error. Each
// service's failure is returned as a note rather than aborting the scan — an
// inventory that refuses to print because IAM was denied tells you nothing about
// the 17 log groups it could have listed.
func (c *Client) ScanControlPlane(ctx context.Context, region string) ([]ControlPlaneResource, []string) {
	cfg := c.regionalConfig(region)
	if region == "" {
		region = cfg.Region
	}
	var out []ControlPlaneResource
	var notes []string

	// --- Lambda functions ---------------------------------------------------
	lc := lambda.NewFromConfig(cfg)
	lp := lambda.NewListFunctionsPaginator(lc, &lambda.ListFunctionsInput{})
	fnNames := map[string]bool{}
	for lp.HasMorePages() {
		page, err := lp.NextPage(ctx)
		if err != nil {
			notes = append(notes, fmt.Sprintf("lambda: %v", err))
			break
		}
		for _, f := range page.Functions {
			name := aws.ToString(f.FunctionName)
			fnNames[name] = true
			if !matchesFootprintName(name) {
				continue
			}
			out = append(out, ControlPlaneResource{
				Service: "lambda", Kind: "function", Name: name, Region: region,
				Detail: fmt.Sprintf("last deployed %s", aws.ToString(f.LastModified)),
				Why:    "name",
			})
		}
	}

	// --- CloudWatch log groups ---------------------------------------------
	//
	// Retention is reported for every row because its ABSENCE was the only
	// control-plane artifact measured to grow without bound (#653): 14 groups
	// holding 238.7 MB, of which the largest was not spawn's at all.
	gc := cwlogs.NewFromConfig(cfg)
	gp := cwlogs.NewDescribeLogGroupsPaginator(gc, &cwlogs.DescribeLogGroupsInput{})
	for gp.HasMorePages() {
		page, err := gp.NextPage(ctx)
		if err != nil {
			notes = append(notes, fmt.Sprintf("logs: %v", err))
			break
		}
		for _, g := range page.LogGroups {
			name := aws.ToString(g.LogGroupName)
			if !matchesFootprintName(name) {
				continue
			}
			r := ControlPlaneResource{
				Service: "logs", Kind: "log-group", Name: name, Region: region,
				Why: "name",
			}
			if g.CreationTime != nil {
				r.Created = time.UnixMilli(*g.CreationTime)
			}
			mb := float64(aws.ToInt64(g.StoredBytes)) / (1024 * 1024)
			if g.RetentionInDays == nil {
				r.Detail = fmt.Sprintf("%.2f MB, retention NONE", mb)
				r.Warn = "no retention: logs are kept forever"
			} else {
				r.Detail = fmt.Sprintf("%.2f MB, retention %dd", mb, *g.RetentionInDays)
			}
			// A log group for a function that no longer exists. Deleting a Lambda
			// does NOT delete its log group — /aws/lambda/github-oauth-bridge
			// outlived the function it belonged to, which is a "leave no trace"
			// failure in its own right (#653).
			if fn := strings.TrimPrefix(name, "/aws/lambda/"); fn != name && len(fnNames) > 0 && !fnNames[fn] {
				r.Warn = "orphaned: no such Lambda function — deleting a function does not delete its log group"
			}
			out = append(out, r)
		}
	}

	// --- EventBridge buses and rules ----------------------------------------
	evRows, evNotes := scanEvents(ctx, eventbridge.NewFromConfig(cfg), region)
	out = append(out, evRows...)
	notes = append(notes, evNotes...)

	// --- EventBridge Scheduler schedules (what `spawn schedule` creates) ------
	schRows, schNotes := scanSchedules(ctx, scheduler.NewFromConfig(cfg), region)
	out = append(out, schRows...)
	notes = append(notes, schNotes...)

	// --- DynamoDB tables ----------------------------------------------------
	dc := dynamodb.NewFromConfig(cfg)
	dp := dynamodb.NewListTablesPaginator(dc, &dynamodb.ListTablesInput{})
	for dp.HasMorePages() {
		page, err := dp.NextPage(ctx)
		if err != nil {
			notes = append(notes, fmt.Sprintf("dynamodb: %v", err))
			break
		}
		for _, t := range page.TableNames {
			if !matchesFootprintName(t) {
				continue
			}
			out = append(out, ControlPlaneResource{
				Service: "dynamodb", Kind: "table", Name: t, Region: region, Why: "name",
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Name < out[j].Name
	})
	return out, notes
}

// rulesLister is the ListRules call listAllRules pages through.
type rulesLister interface {
	ListRules(ctx context.Context, in *eventbridge.ListRulesInput, optFns ...func(*eventbridge.Options)) (*eventbridge.ListRulesOutput, error)
}

// eventsScanner is what scanEvents needs: the buses, and each bus's rules.
type eventsScanner interface {
	rulesLister
	ListEventBuses(ctx context.Context, in *eventbridge.ListEventBusesInput, optFns ...func(*eventbridge.Options)) (*eventbridge.ListEventBusesOutput, error)
}

// scanEvents enumerates spore.host's EventBridge footprint in one region:
// custom event buses by name, and rules by name on EVERY bus, default and
// custom. Only the default bus was read before #1287, which put the TTL
// reaper's instance-event bus and the rule on it out of sight; a footprint
// that cannot see a resource cannot report it left behind.
//
// EventBridge Scheduler schedules are a different service; scanSchedules
// covers them.
func scanEvents(ctx context.Context, c eventsScanner, region string) ([]ControlPlaneResource, []string) {
	var out []ControlPlaneResource
	var notes []string
	buses := []string{""} // the default bus
	if bs, err := listAllBuses(ctx, c); err != nil {
		notes = append(notes, fmt.Sprintf("events buses: %v", err))
	} else {
		for _, b := range bs {
			name := aws.ToString(b.Name)
			if name == "" || name == "default" {
				continue
			}
			buses = append(buses, name)
			if matchesFootprintName(name) {
				out = append(out, ControlPlaneResource{
					Service: "events", Kind: "event-bus", Name: name, Region: region, Why: "name",
				})
			}
		}
	}
	for _, bus := range buses {
		rules, err := listAllRules(ctx, c, bus)
		if err != nil {
			notes = append(notes, fmt.Sprintf("events: %v", err))
			continue
		}
		for _, r := range rules {
			name := aws.ToString(r.Name)
			if !matchesFootprintName(name) {
				continue
			}
			detail := aws.ToString(r.ScheduleExpression)
			if detail == "" {
				detail = "event pattern"
			}
			detail = fmt.Sprintf("%s, %s", detail, r.State)
			if bus != "" {
				detail += ", on bus " + bus
			}
			out = append(out, ControlPlaneResource{
				Service: "events", Kind: "rule", Name: name, Region: region, Detail: detail, Why: "name",
			})
		}
	}
	return out, notes
}

// schedulesScanner is what scanSchedules needs from EventBridge Scheduler.
type schedulesScanner interface {
	ListSchedules(ctx context.Context, in *scheduler.ListSchedulesInput, optFns ...func(*scheduler.Options)) (*scheduler.ListSchedulesOutput, error)
	GetSchedule(ctx context.Context, in *scheduler.GetScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.GetScheduleOutput, error)
}

// spawnScheduleID is the shape of the names `spawn schedule` gives its
// schedules (pkg/scheduler.GenerateScheduleID: sched-YYYYMMDD-HHMMSS).
var spawnScheduleID = regexp.MustCompile(`^sched-\d{8}-\d{6}$`)

// scanSchedules enumerates spore.host's EventBridge Scheduler schedules in one
// region, in every schedule group (#1219). `spawn schedule` names them
// sched-YYYYMMDD-HHMMSS in the default group, which no name prefix matches, so
// a schedule is identified by, in order:
//
//	target       its target is a spore.host Lambda (scheduler-handler, by
//	             footprintExactNames)
//	name         its name or its group carries a spore.host prefix
//	description  it has spawn's ID shape and its Description opens with
//	             pkg/scheduler.ScheduleDescriptionPrefix (one GetSchedule each,
//	             only for those candidates: the summary carries no description)
//
// A schedule that outlives its intent keeps launching instances, so a
// footprint that cannot see schedules hides the one control-plane resource
// that spends. scheduler:ListSchedules (and GetSchedule for the description
// check) is not in every principal's grant; a refusal becomes a note naming
// the permission, never an abort.
func scanSchedules(ctx context.Context, c schedulesScanner, region string) ([]ControlPlaneResource, []string) {
	var out []ControlPlaneResource
	var notes []string
	in := &scheduler.ListSchedulesInput{} // no GroupName: every group
	for {
		page, err := c.ListSchedules(ctx, in)
		if err != nil {
			notes = append(notes, fmt.Sprintf("scheduler: %v (listing schedules needs scheduler:ListSchedules)", err))
			break
		}
		for _, s := range page.Schedules {
			name := aws.ToString(s.Name)
			group := aws.ToString(s.GroupName)
			target := ""
			if s.Target != nil {
				target = aws.ToString(s.Target.Arn)
			}
			var why string
			switch {
			case matchesFootprintName(lambdaFunctionFromARN(target)):
				why = "target"
			case matchesFootprintName(name) || matchesFootprintName(group):
				why = "name"
			case spawnScheduleID.MatchString(name):
				got, gerr := c.GetSchedule(ctx, &scheduler.GetScheduleInput{Name: s.Name, GroupName: s.GroupName})
				if gerr != nil {
					notes = append(notes, fmt.Sprintf("scheduler: schedule %s/%s: %v (reading its description needs scheduler:GetSchedule)", group, name, gerr))
					continue
				}
				if !strings.HasPrefix(aws.ToString(got.Description), spawnscheduler.ScheduleDescriptionPrefix) {
					continue
				}
				why = "description"
			default:
				continue
			}
			detail := fmt.Sprintf("%s, group %s", s.State, group)
			if target != "" {
				detail += ", target " + target
			}
			r := ControlPlaneResource{
				Service: "scheduler", Kind: "schedule", Name: name, Region: region, Detail: detail, Why: why,
			}
			if s.CreationDate != nil {
				r.Created = *s.CreationDate
			}
			out = append(out, r)
		}
		if aws.ToString(page.NextToken) == "" {
			break
		}
		in = &scheduler.ListSchedulesInput{NextToken: page.NextToken}
	}
	return out, notes
}

// lambdaFunctionFromARN is the function name in a Lambda ARN
// (arn:aws:lambda:REGION:ACCOUNT:function:NAME[:QUALIFIER]), or "" for any
// other ARN.
func lambdaFunctionFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 7 || parts[2] != "lambda" || parts[5] != "function" {
		return ""
	}
	return parts[6]
}

// listAllBuses reads every page of ListEventBuses (no SDK paginator either).
func listAllBuses(ctx context.Context, c eventsScanner) ([]ebtypes.EventBus, error) {
	var all []ebtypes.EventBus
	in := &eventbridge.ListEventBusesInput{}
	for {
		page, err := c.ListEventBuses(ctx, in)
		if err != nil {
			return all, err
		}
		all = append(all, page.EventBuses...)
		if aws.ToString(page.NextToken) == "" {
			return all, nil
		}
		in = &eventbridge.ListEventBusesInput{NextToken: page.NextToken}
	}
}

// listAllRules reads every page of one bus's rules ("" is the default bus).
// The SDK has no paginator for ListRules, so the NextToken loop is written out.
func listAllRules(ctx context.Context, c rulesLister, bus string) ([]ebtypes.Rule, error) {
	var all []ebtypes.Rule
	in := &eventbridge.ListRulesInput{}
	if bus != "" {
		in.EventBusName = aws.String(bus)
	}
	for {
		page, err := c.ListRules(ctx, in)
		if err != nil {
			return all, err
		}
		all = append(all, page.Rules...)
		if aws.ToString(page.NextToken) == "" {
			return all, nil
		}
		in = &eventbridge.ListRulesInput{NextToken: page.NextToken, EventBusName: in.EventBusName}
	}
}

// ScanGlobalFootprint enumerates the account-wide (non-regional) control plane:
// IAM roles and instance profiles, and S3 buckets.
//
// Separate from ScanControlPlane because these are global, and listing them once
// per region would report an 11-bucket family eleven times.
func (c *Client) ScanGlobalFootprint(ctx context.Context) ([]ControlPlaneResource, []string) {
	cfg := c.regionalConfig("")
	var out []ControlPlaneResource
	var notes []string

	ic := iam.NewFromConfig(cfg)
	rp := iam.NewListRolesPaginator(ic, &iam.ListRolesInput{})
	for rp.HasMorePages() {
		page, err := rp.NextPage(ctx)
		if err != nil {
			notes = append(notes, fmt.Sprintf("iam roles: %v", err))
			break
		}
		for _, r := range page.Roles {
			name := aws.ToString(r.RoleName)
			if !matchesFootprintName(name) {
				continue
			}
			res := ControlPlaneResource{
				Service: "iam", Kind: "role", Name: name, Why: "name",
			}
			if r.CreateDate != nil {
				res.Created = *r.CreateDate
			}
			out = append(out, res)
		}
	}

	pp := iam.NewListInstanceProfilesPaginator(ic, &iam.ListInstanceProfilesInput{})
	for pp.HasMorePages() {
		page, err := pp.NextPage(ctx)
		if err != nil {
			notes = append(notes, fmt.Sprintf("iam instance profiles: %v", err))
			break
		}
		for _, p := range page.InstanceProfiles {
			name := aws.ToString(p.InstanceProfileName)
			if !matchesFootprintName(name) {
				continue
			}
			res := ControlPlaneResource{
				Service: "iam", Kind: "instance-profile", Name: name, Why: "name",
			}
			if p.CreateDate != nil {
				res.Created = *p.CreateDate
			}
			out = append(out, res)
		}
	}

	sc := s3.NewFromConfig(cfg)
	if bs, err := sc.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		notes = append(notes, fmt.Sprintf("s3: %v", err))
	} else {
		for _, b := range bs.Buckets {
			name := aws.ToString(b.Name)
			if !matchesFootprintName(name) {
				continue
			}
			res := ControlPlaneResource{
				Service: "s3", Kind: "bucket", Name: name, Why: "name",
			}
			if b.CreationDate != nil {
				res.Created = *b.CreationDate
			}
			out = append(out, res)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Name < out[j].Name
	})
	return out, notes
}
