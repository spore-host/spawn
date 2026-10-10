package aws

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	Service string `json:"service"` // lambda, logs, s3, iam, events, dynamodb
	Kind    string `json:"kind"`    // function, log-group, bucket, role, instance-profile, rule, table
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

	// --- EventBridge rules --------------------------------------------------
	ec := eventbridge.NewFromConfig(cfg)
	if rules, err := ec.ListRules(ctx, &eventbridge.ListRulesInput{}); err != nil {
		notes = append(notes, fmt.Sprintf("events: %v", err))
	} else {
		for _, r := range rules.Rules {
			name := aws.ToString(r.Name)
			if !matchesFootprintName(name) {
				continue
			}
			out = append(out, ControlPlaneResource{
				Service: "events", Kind: "rule", Name: name, Region: region,
				Detail: fmt.Sprintf("%s, %s", aws.ToString(r.ScheduleExpression), r.State),
				Why:    "name",
			})
		}
	}

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
