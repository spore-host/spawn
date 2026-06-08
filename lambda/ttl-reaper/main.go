// Command ttl-reaper is a scheduled, out-of-band backstop that terminates
// spawn-managed EC2 instances whose lifetime has expired.
//
// Why this exists (#70): instance lifetime is normally enforced from *inside*
// the instance by the spored daemon's monitor loop. #65 showed that when that
// loop silently dies, TTL/idle/on-complete/pre-stop all stop — and instances
// run forever. This reaper enforces the same deadline from the outside, so a
// spore can never outlive its deadline regardless of spored's health. It is a
// BACKSTOP, not a replacement: spored remains the primary, graceful enforcer.
//
// It terminates an instance when EITHER:
//   - now > spawn:ttl-deadline (the authoritative launch-anchored deadline), OR
//   - now - spawn:launch-time > REAPER_MAX_AGE (a hard ceiling that fires even
//     for --no-timeout / missing / far-future deadlines — defense against
//     escape hatches and tag tampering).
//
// It scans running AND stopped instances: an idle-stopped instance runs no
// daemon, so its TTL can never fire locally (#71) — the reaper is the only
// thing that will ever reap it.
//
// Safety: REAPER_DRY_RUN=true logs "WOULD reap" without terminating. Every reap
// (real or dry-run) is posted to REAPER_NOTIFY_URL so reaps are never silent.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// defaultRegions is the set scanned when REAPER_REGIONS is unset. It mirrors the
// 11 regions the release workflow publishes spored to (spawn-binaries-<region>),
// i.e. where spawn actually operates. Override via REAPER_REGIONS for more/fewer.
var defaultRegions = []string{
	"us-east-1", "us-east-2", "us-west-1", "us-west-2",
	"ca-central-1",
	"eu-west-1", "eu-west-2", "eu-central-1",
	"ap-southeast-1", "ap-southeast-2", "ap-northeast-1",
}

const defaultMaxAge = 7 * 24 * time.Hour

// account is one spore-launching AWS account the reaper scans. label is for
// logging; ec2For returns a regional EC2 client using that account's creds
// (the Lambda's own creds for the local account, or assumed cross-account creds).
type account struct {
	label  string
	ec2For func(region string) *ec2.Client
}

// reaper holds resolved configuration. Spores can be launched into ANY account a
// user points spawn at (the account is decided by the caller's credentials), so
// the reaper scans a configured set of accounts × regions — not just one.
type reaper struct {
	accounts  []account
	regions   []string
	maxAge    time.Duration
	dryRun    bool
	notifyURL string
}

var r *reaper

func init() {
	tagprefix.Init() // respect SPORED_TAG_PREFIX, matching how spawn writes tags

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS config: %v", err)
	}

	r = &reaper{
		accounts:  resolveAccounts(ctx, cfg),
		regions:   parseRegions(os.Getenv("REAPER_REGIONS")),
		maxAge:    parseMaxAge(os.Getenv("REAPER_MAX_AGE")),
		dryRun:    strings.EqualFold(os.Getenv("REAPER_DRY_RUN"), "true"),
		notifyURL: strings.TrimRight(os.Getenv("REAPER_NOTIFY_URL"), "/"),
	}
	labels := make([]string, len(r.accounts))
	for i, a := range r.accounts {
		labels[i] = a.label
	}
	log.Printf("ttl-reaper initialized (accounts=%v, regions=%v, max-age=%s, dry-run=%t)",
		labels, r.regions, r.maxAge, r.dryRun)
}

// resolveAccounts builds the list of accounts to scan from configuration:
//   - REAPER_ROLE_ARNS: comma-separated cross-account role ARNs, one per
//     spore-launching account. Each is assumed (with EC2_EXTERNAL_ID).
//   - REAPER_SCAN_SELF=true: also scan the Lambda's OWN account directly (no
//     assume-role) — for deployments where spores run in the infra account too.
//
// Back-compat: the singular EC2_ROLE_ARN is accepted and folded into the list.
func resolveAccounts(ctx context.Context, base aws.Config) []account {
	externalID := os.Getenv("EC2_EXTERNAL_ID")
	stsClient := sts.NewFromConfig(base)

	var accounts []account
	if strings.EqualFold(os.Getenv("REAPER_SCAN_SELF"), "true") {
		accounts = append(accounts, account{
			label: "self",
			ec2For: func(region string) *ec2.Client {
				c := base.Copy()
				c.Region = region
				return ec2.NewFromConfig(c)
			},
		})
	}

	roleARNs := parseList(os.Getenv("REAPER_ROLE_ARNS"))
	if legacy := strings.TrimSpace(os.Getenv("EC2_ROLE_ARN")); legacy != "" {
		roleARNs = append(roleARNs, legacy)
	}
	for _, roleARN := range dedup(roleARNs) {
		roleARN := roleARN
		log.Printf("will assume cross-account EC2 role: %s", roleARN)
		creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = "spawn-ttl-reaper"
			if externalID != "" {
				o.ExternalID = aws.String(externalID)
			}
		})
		acctCfg, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(creds))
		if err != nil {
			log.Fatalf("create cross-account config for %s: %v", roleARN, err)
		}
		accounts = append(accounts, account{
			label: accountIDFromRoleARN(roleARN),
			ec2For: func(region string) *ec2.Client {
				c := acctCfg.Copy()
				c.Region = region
				return ec2.NewFromConfig(c)
			},
		})
	}

	if len(accounts) == 0 {
		// No roles and no scan-self: fall back to scanning the local account so
		// the reaper is never a no-op by misconfiguration.
		log.Printf("no REAPER_ROLE_ARNS/EC2_ROLE_ARN and REAPER_SCAN_SELF!=true; defaulting to local account")
		accounts = append(accounts, account{
			label: "self",
			ec2For: func(region string) *ec2.Client {
				c := base.Copy()
				c.Region = region
				return ec2.NewFromConfig(c)
			},
		})
	}
	return accounts
}

func parseList(env string) []string {
	var out []string
	for _, p := range strings.Split(env, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// accountIDFromRoleARN extracts the 12-digit account id from an IAM role ARN
// (arn:aws:iam::ACCOUNT:role/NAME) for logging; returns the ARN if it can't.
func accountIDFromRoleARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) >= 5 && len(parts[4]) == 12 {
		return parts[4]
	}
	return arn
}

func parseRegions(env string) []string {
	if env == "" {
		return defaultRegions
	}
	var out []string
	for _, p := range strings.Split(env, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return defaultRegions
	}
	return out
}

func parseMaxAge(env string) time.Duration {
	if env == "" {
		return defaultMaxAge
	}
	d, err := time.ParseDuration(env)
	if err != nil || d <= 0 {
		log.Printf("invalid REAPER_MAX_AGE %q, using default %s", env, defaultMaxAge)
		return defaultMaxAge
	}
	return d
}

// Summary is returned to CloudWatch and is handy for assertions in tests.
type Summary struct {
	Accounts int `json:"accounts"`
	Scanned  int `json:"scanned"`
	Expired  int `json:"expired"`
	Reaped   int `json:"reaped"`
	Skipped  int `json:"skipped"` // expired-but-not-terminated (dry-run)
	Errors   int `json:"errors"`
}

// candidate is an instance the reaper decided should die, with the reason.
type candidate struct {
	id       string
	name     string
	account  string
	region   string
	reason   string // human-readable: "ttl-deadline" or "max-age"
	deadline string // the deadline/launch-time that triggered it
	age      time.Duration
}

func handler(ctx context.Context) (Summary, error) {
	start := time.Now().UTC()
	sum := Summary{Accounts: len(r.accounts)}

	// Scan every spore-launching account × region. A spore lands in whatever
	// account the caller's credentials pointed at, so coverage must be per-account.
	for _, acct := range r.accounts {
		for _, region := range r.regions {
			cands, scanned, err := r.scanRegion(ctx, acct, region, start)
			sum.Scanned += scanned
			if err != nil {
				log.Printf("account %s region %s: scan error: %v", acct.label, region, err)
				sum.Errors++
				continue
			}
			sum.Expired += len(cands)
			for _, c := range cands {
				if r.dryRun {
					log.Printf("WOULD reap %s (%s) in %s/%s — %s (age %s, deadline %s)",
						c.id, c.name, c.account, c.region, c.reason, c.age.Round(time.Minute), c.deadline)
					r.notify(c, true)
					sum.Skipped++
					continue
				}
				if err := r.terminate(ctx, acct, c); err != nil {
					log.Printf("account %s region %s: terminate %s failed: %v", acct.label, region, c.id, err)
					sum.Errors++
					continue
				}
				log.Printf("REAPED %s (%s) in %s/%s — %s (age %s, deadline %s)",
					c.id, c.name, c.account, c.region, c.reason, c.age.Round(time.Minute), c.deadline)
				r.notify(c, false)
				sum.Reaped++
			}
		}
	}

	log.Printf("ttl-reaper done in %s: %+v", time.Since(start).Round(time.Millisecond), sum)
	return sum, nil
}

// scanRegion lists spawn-managed instances (running AND stopped) in one
// account+region and returns those past their deadline or the max-age ceiling.
func (r *reaper) scanRegion(ctx context.Context, acct account, region string, now time.Time) ([]candidate, int, error) {
	client := acct.ec2For(region)
	var cands []candidate
	scanned := 0

	paginator := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String(tagprefix.FilterTag("managed")), Values: []string{"true"}},
			// Include stopped/stopping: an idle-stopped instance runs no daemon,
			// so only the reaper will ever reclaim it (#71). Exclude terminated/
			// shutting-down (already dying).
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, scanned, fmt.Errorf("describe instances: %w", err)
		}
		for _, res := range page.Reservations {
			for _, inst := range res.Instances {
				scanned++
				if c, expired := r.evaluate(inst, region, now); expired {
					c.account = acct.label
					cands = append(cands, c)
				}
			}
		}
	}
	return cands, scanned, nil
}

// evaluate decides whether a single instance is past its deadline or max-age.
func (r *reaper) evaluate(inst ec2types.Instance, region string, now time.Time) (candidate, bool) {
	tags := tagMap(inst.Tags)
	id := aws.ToString(inst.InstanceId)
	name := tags["Name"]
	if name == "" {
		name = tags[tagprefix.Tag("dns-name")]
	}

	// 1. spawn:ttl-deadline — the authoritative, launch-anchored deadline.
	if dl := tags[tagprefix.Tag("ttl-deadline")]; dl != "" {
		if deadline, err := time.Parse(time.RFC3339, dl); err == nil {
			if now.After(deadline) {
				return candidate{
					id: id, name: name, region: region,
					reason: "ttl-deadline", deadline: dl, age: now.Sub(deadline),
				}, true
			}
			// Within deadline — respect it; do NOT also apply max-age below
			// (the deadline is the user's explicit, honored intent).
			return candidate{}, false
		}
		log.Printf("instance %s: unparseable ttl-deadline %q, falling back to max-age", id, dl)
	}

	// 2. Hard max-age ceiling — fires for --no-timeout / missing / unparseable
	// deadlines. Anchored to spawn:launch-time, falling back to EC2 LaunchTime.
	launch := launchTime(tags[tagprefix.Tag("launch-time")], inst.LaunchTime)
	if !launch.IsZero() && now.Sub(launch) > r.maxAge {
		return candidate{
			id: id, name: name, region: region,
			reason: "max-age", deadline: launch.UTC().Format(time.RFC3339), age: now.Sub(launch),
		}, true
	}
	return candidate{}, false
}

func (r *reaper) terminate(ctx context.Context, acct account, c candidate) error {
	client := acct.ec2For(c.region)
	_, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{c.id},
	})
	if err != nil {
		// Already gone is success (idempotent).
		if strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
			return nil
		}
		return err
	}
	return nil
}

// notify posts a plain Slack-incoming-webhook payload ({"text":...}) to
// REAPER_NOTIFY_URL. Unlike the in-instance notifier, this needs no IMDS
// identity document, so it works from Lambda. Best-effort; never blocks a reap.
func (r *reaper) notify(c candidate, dryRun bool) {
	if r.notifyURL == "" {
		return
	}
	verb := "🪓 Reaped"
	if dryRun {
		verb = "🔎 [dry-run] would reap"
	}
	text := fmt.Sprintf("%s spore `%s` (%s) in %s/%s — %s expired, age %s (deadline %s)",
		verb, c.name, c.id, c.account, c.region, c.reason, c.age.Round(time.Minute), c.deadline)

	body, _ := json.Marshal(map[string]string{"text": text})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", r.notifyURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("notify: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		log.Printf("notify: post: %v", err)
		return
	}
	_ = resp.Body.Close()
}

func tagMap(tags []ec2types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// launchTime prefers the spawn:launch-time tag (RFC3339, anchored at launch and
// stable across stop/start) and falls back to the EC2 API LaunchTime, which is
// reset on each start and so is only a loose lower bound.
func launchTime(tagVal string, apiLaunch *time.Time) time.Time {
	if tagVal != "" {
		if t, err := time.Parse(time.RFC3339, tagVal); err == nil {
			return t
		}
	}
	if apiLaunch != nil {
		return *apiLaunch
	}
	return time.Time{}
}

func main() {
	lambda.Start(handler)
}
