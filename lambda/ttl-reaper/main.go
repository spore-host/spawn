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
//
// Graceful tier (#187, opt-in via REAPER_GRACEFUL=true): before the hard
// terminate, the reaper can try to flush a running instance's --pre-stop hook
// via SSM RunCommand (run as spawn:local-username, per #63), bounded by
// REAPER_GRACEFUL_MAX_WAIT. This matters only when spored itself failed to run
// pre-stop (dead/wedged) — when spored is healthy it already flushed. The
// attempt is strictly best-effort and never blocks or prevents the terminate, so
// the hard-deadline guarantee (#72) is preserved.
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
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spore-host/spawn/pkg/dns"
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
// logging; ec2For/ssmFor return a regional EC2/SSM client using that account's
// creds (the Lambda's own creds for the local account, or assumed cross-account
// creds). ssmFor is used by the graceful pre-stop tier (#187).
type account struct {
	label     string
	accountID string // 12-digit AWS account ID (for the {base36}.domain subdomain)
	ec2For    func(region string) *ec2.Client
	ssmFor    func(region string) *ssm.Client
	fsxFor    func(region string) *fsx.Client
}

// reaper holds resolved configuration. Spores can be launched into ANY account a
// user points spawn at (the account is decided by the caller's credentials), so
// the reaper scans a configured set of accounts × regions — not just one.
type reaper struct {
	accounts     []account
	regions      []string
	maxAge       time.Duration
	dryRun       bool
	notifyURL    string
	graceful     bool          // REAPER_GRACEFUL=true: attempt a pre-stop flush via SSM before terminate (#187)
	gracefulWait time.Duration // hard cap on the per-instance graceful flush (REAPER_GRACEFUL_MAX_WAIT)

	// DNS teardown (#247): the Route53 zone the reaper cleans up after a reap.
	// The zone lives in the reaper's OWN (infra) account — not the per-instance
	// cross-account role — so route53Client uses the base credentials. dnsDomain
	// is the zone's domain (e.g. "spore.host"); both empty disables DNS teardown.
	route53Client *route53.Client
	dnsZoneID     string
	dnsDomain     string

	// DNS reconciliation sweep (#438, opt-in via REAPER_DNS_SWEEP=true). The
	// #247 teardown only deletes a record when the reaper still SEES the instance
	// via DescribeInstances. A record whose instance died abruptly (hard crash,
	// out-of-band terminate, fast spot reclaim) and has since aged out of the EC2
	// API is orphaned — nothing keys off it, so it resolves forever to a dead (or
	// later reassigned) IP. The sweep reconciles the zone against live instance IPs
	// and deletes A-records with no live owner. Requires a configured zone.
	sweepDNS bool
}

// defaultGracefulWait caps how long the reaper waits for a single instance's
// pre-stop flush. The reaper is a backstop, not a babysitter — keep it short.
const defaultGracefulWait = 2 * time.Minute

// ephemeralOrphanGrace is how long an ephemeral FSx may exist with NO instance
// referencing it (neither spawn:fsx-id nor spawn:fsx-pending) before the reaper
// treats it as orphaned and reclaims it (#210). An ephemeral FSx is created
// BEFORE its instance launches; if the launch fails (e.g. InsufficientInstance
// Capacity, the case lagotto retries on) no instance ever owns it, so the
// refcount→0 reclamation keyed on instance termination never fires and the
// filesystem orphans as a billable 1.2 TiB resource. This grace covers that hole
// while comfortably clearing the normal create→launch→tag window (RunInstances is
// seconds; spored stamps spawn:fsx-pending immediately). Kept well under the FSx
// AVAILABLE time (~10 min) so a healthy filesystem is always referenced before
// the grace elapses.
const ephemeralOrphanGrace = 30 * time.Minute

var r *reaper

func init() {
	tagprefix.Init() // respect SPORED_TAG_PREFIX, matching how spawn writes tags

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS config: %v", err)
	}

	r = &reaper{
		accounts:     resolveAccounts(ctx, cfg),
		regions:      parseRegions(os.Getenv("REAPER_REGIONS")),
		maxAge:       parseMaxAge(os.Getenv("REAPER_MAX_AGE")),
		dryRun:       strings.EqualFold(os.Getenv("REAPER_DRY_RUN"), "true"),
		notifyURL:    strings.TrimRight(os.Getenv("REAPER_NOTIFY_URL"), "/"),
		graceful:     strings.EqualFold(os.Getenv("REAPER_GRACEFUL"), "true"),
		gracefulWait: parseGracefulWait(os.Getenv("REAPER_GRACEFUL_MAX_WAIT")),
		dnsZoneID:    strings.TrimSpace(os.Getenv("REAPER_DNS_ZONE_ID")),
		dnsDomain:    strings.TrimSpace(os.Getenv("REAPER_DNS_DOMAIN")),
		sweepDNS:     strings.EqualFold(os.Getenv("REAPER_DNS_SWEEP"), "true"),
	}
	// Route53 lives in the reaper's own account; use base creds. Only wire the
	// client when a zone is configured, so a deployment without DNS teardown
	// stays a no-op (and needs no route53 IAM).
	if r.dnsZoneID != "" && r.dnsDomain != "" {
		r.route53Client = route53.NewFromConfig(cfg)
	} else {
		log.Printf("DNS teardown disabled (REAPER_DNS_ZONE_ID/REAPER_DNS_DOMAIN unset)")
	}
	labels := make([]string, len(r.accounts))
	for i, a := range r.accounts {
		labels[i] = a.label
	}
	if r.sweepDNS && r.route53Client == nil {
		log.Printf("REAPER_DNS_SWEEP=true but no zone configured (REAPER_DNS_ZONE_ID/REAPER_DNS_DOMAIN) — sweep disabled")
		r.sweepDNS = false
	}
	log.Printf("ttl-reaper initialized (accounts=%v, regions=%v, max-age=%s, dry-run=%t, graceful=%t, graceful-wait=%s, dns-sweep=%t)",
		labels, r.regions, r.maxAge, r.dryRun, r.graceful, r.gracefulWait, r.sweepDNS)
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

	// selfAccountID resolves the reaper's own account ID (for the "self" account's
	// DNS subdomain). Best-effort: an error just leaves it empty, which disables
	// the sweep for the self account (never a hard failure).
	selfAccountID := func() string {
		out, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			log.Printf("resolve self account id: %v (DNS sweep disabled for self account)", err)
			return ""
		}
		return aws.ToString(out.Account)
	}

	var accounts []account
	if strings.EqualFold(os.Getenv("REAPER_SCAN_SELF"), "true") {
		accounts = append(accounts, account{
			label:     "self",
			accountID: selfAccountID(),
			ec2For: func(region string) *ec2.Client {
				c := base.Copy()
				c.Region = region
				return ec2.NewFromConfig(c)
			},
			ssmFor: func(region string) *ssm.Client {
				c := base.Copy()
				c.Region = region
				return ssm.NewFromConfig(c)
			},
			fsxFor: func(region string) *fsx.Client {
				c := base.Copy()
				c.Region = region
				return fsx.NewFromConfig(c)
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
			label:     accountIDFromRoleARN(roleARN),
			accountID: accountIDFromRoleARN(roleARN),
			ec2For: func(region string) *ec2.Client {
				c := acctCfg.Copy()
				c.Region = region
				return ec2.NewFromConfig(c)
			},
			ssmFor: func(region string) *ssm.Client {
				c := acctCfg.Copy()
				c.Region = region
				return ssm.NewFromConfig(c)
			},
			fsxFor: func(region string) *fsx.Client {
				c := acctCfg.Copy()
				c.Region = region
				return fsx.NewFromConfig(c)
			},
		})
	}

	if len(accounts) == 0 {
		// No roles and no scan-self: fall back to scanning the local account so
		// the reaper is never a no-op by misconfiguration.
		log.Printf("no REAPER_ROLE_ARNS/EC2_ROLE_ARN and REAPER_SCAN_SELF!=true; defaulting to local account")
		accounts = append(accounts, account{
			label:     "self",
			accountID: selfAccountID(),
			ec2For: func(region string) *ec2.Client {
				c := base.Copy()
				c.Region = region
				return ec2.NewFromConfig(c)
			},
			ssmFor: func(region string) *ssm.Client {
				c := base.Copy()
				c.Region = region
				return ssm.NewFromConfig(c)
			},
			fsxFor: func(region string) *fsx.Client {
				c := base.Copy()
				c.Region = region
				return fsx.NewFromConfig(c)
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

func parseGracefulWait(env string) time.Duration {
	if env == "" {
		return defaultGracefulWait
	}
	d, err := time.ParseDuration(env)
	if err != nil || d <= 0 {
		log.Printf("invalid REAPER_GRACEFUL_MAX_WAIT %q, using default %s", env, defaultGracefulWait)
		return defaultGracefulWait
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

	// FSx (#192): orphaned spawn-managed FSx Lustre filesystems reclaimed.
	FSxScanned int `json:"fsx_scanned"`
	FSxExpired int `json:"fsx_expired"` // refcount 0 AND past deadline/max-age
	FSxReaped  int `json:"fsx_reaped"`
	FSxSkipped int `json:"fsx_skipped"` // would-reap (dry-run) or held back (still in use / unflushed)

	// DNS sweep (#438): orphaned Route53 A-records reconciled against live IPs.
	DNSScanned int `json:"dns_scanned"` // A-records examined under the account subdomains
	DNSReaped  int `json:"dns_reaped"`  // orphaned records deleted (or would-delete in dry-run)
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

	// Graceful-tier inputs (#187): if the instance is running and has a pre-stop
	// hook, the reaper tries to flush it via SSM before the hard terminate.
	running        bool
	preStop        string        // spawn:pre-stop hook command (empty = nothing to flush)
	preStopTimeout time.Duration // spawn:pre-stop-timeout (0 = use default)
	localUsername  string        // spawn:local-username (run the hook as this user, #63)

	// DNS-teardown inputs (#247): the reaper deletes the instance's Route53
	// records itself, because spored's graceful DeleteDNS never ran (the reaper
	// only fires when spored failed). dnsName + accountBase36 give the canonical
	// A-record FQDN; accountName (if set) gives the #121 alias CNAME to also remove.
	dnsName       string // spawn:dns-name (empty = the instance registered no DNS)
	accountBase36 string // spawn:account-base36 (the A-record's account subdomain)
	accountName   string // spawn:account-name (the alias CNAME subdomain, #121; optional)
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
					r.deleteDNS(ctx, c) // dry-run aware: logs "WOULD delete DNS …"
					r.notify(c, true)
					sum.Skipped++
					continue
				}
				// Graceful tier (#187): when enabled, try to flush the instance's
				// pre-stop hook via SSM before the hard kill — for instances spored
				// failed to reap gracefully itself. Strictly best-effort and bounded;
				// the terminate below ALWAYS runs regardless of the outcome, so the
				// hard-deadline guarantee (#72) is preserved.
				if r.graceful {
					r.tryGracefulPreStop(ctx, acct, c)
				}
				if err := r.terminate(ctx, acct, c); err != nil {
					log.Printf("account %s region %s: terminate %s failed: %v", acct.label, region, c.id, err)
					sum.Errors++
					continue
				}
				// Manual DNS teardown (#247): spored's graceful DeleteDNS never ran
				// (the reaper only fires when spored failed), so the reaper deletes
				// the instance's Route53 records itself. Best-effort, after the
				// terminate so the hard-deadline guarantee is never blocked by it.
				r.deleteDNS(ctx, c)
				log.Printf("REAPED %s (%s) in %s/%s — %s (age %s, deadline %s)",
					c.id, c.name, c.account, c.region, c.reason, c.age.Round(time.Minute), c.deadline)
				r.notify(c, false)
				sum.Reaped++
			}

			// FSx reclamation (#192): reclaim orphaned spawn-managed FSx
			// filesystems — those past their deadline/max-age with NO live
			// instance still using them (refcount 0). Independent of the instance
			// scan above; an FSx outlives its instances by design.
			r.reapFSxRegion(ctx, acct, region, start, &sum)
		}

		// DNS reconciliation sweep (#438): after scanning all regions for this
		// account, reconcile the account's {base36}.{domain} A-records against the
		// set of live instance public IPs and delete records with no live owner.
		// This catches records orphaned by abrupt exits (crash / out-of-band
		// terminate / fast spot reclaim) that the instance-driven #247 teardown
		// can't — the instance is already gone from the EC2 API. Opt-in + best-
		// effort; never affects the terminate path above.
		if r.sweepDNS {
			r.sweepAccountDNS(ctx, acct, &sum)
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

	// Base candidate carries the graceful-tier inputs (#187), shared by both the
	// deadline and max-age branches.
	base := candidate{
		id: id, name: name, region: region,
		running:       inst.State != nil && inst.State.Name == ec2types.InstanceStateNameRunning,
		preStop:       tags[tagprefix.Tag("pre-stop")],
		localUsername: tags[tagprefix.Tag("local-username")],
		dnsName:       tags[tagprefix.Tag("dns-name")],
		accountBase36: tags[tagprefix.Tag("account-base36")],
		accountName:   tags[tagprefix.Tag("account-name")],
	}
	if pt := tags[tagprefix.Tag("pre-stop-timeout")]; pt != "" {
		if d, err := time.ParseDuration(pt); err == nil {
			base.preStopTimeout = d
		}
	}

	// 1. spawn:ttl-deadline — the authoritative, launch-anchored deadline.
	if dl := tags[tagprefix.Tag("ttl-deadline")]; dl != "" {
		if deadline, err := time.Parse(time.RFC3339, dl); err == nil {
			if now.After(deadline) {
				c := base
				c.reason, c.deadline, c.age = "ttl-deadline", dl, now.Sub(deadline)
				return c, true
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
		c := base
		c.reason, c.deadline, c.age = "max-age", launch.UTC().Format(time.RFC3339), now.Sub(launch)
		return c, true
	}
	return candidate{}, false
}

// reapFSxRegion scans spawn-managed FSx Lustre filesystems in one account+region
// and reclaims those that are orphaned (#192): past their deadline/max-age AND
// no live instance still references them (refcount 0). It mirrors the instance
// path's dry-run / notify behavior and updates the FSx counters on sum.
//
// Safety properties:
//   - refcount: an FSx with ANY live instance tagged spawn:fsx-id=<this fs> is
//     never reaped, regardless of deadline — the in-use lease wins.
//   - deadline: only filesystems past spawn:ttl-deadline (or, lacking one, older
//     than max-age) are eligible, same logic as instances.
//   - export-flush: a filesystem with an export DRA still catching up is held
//     back (counted Skipped), so we never delete data that hasn't reached S3 —
//     the #184 lesson. DeleteFileSystem then runs WITHOUT SkipFinalExport so any
//     remaining changes flush on delete.
func (r *reaper) reapFSxRegion(ctx context.Context, acct account, region string, now time.Time, sum *Summary) {
	if acct.fsxFor == nil {
		return
	}
	fsxClient := acct.fsxFor(region)

	paginator := fsx.NewDescribeFileSystemsPaginator(fsxClient, &fsx.DescribeFileSystemsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Printf("account %s region %s: describe filesystems: %v", acct.label, region, err)
			sum.Errors++
			return
		}
		for i := range page.FileSystems {
			fsItem := page.FileSystems[i]
			sum.FSxScanned++

			id, reason, deadline, expired := r.evaluateFSx(fsItem, now)
			if !expired {
				continue
			}
			sum.FSxExpired++

			// Refcount: any live instance still using this filesystem?
			inUse, err := r.fsxInUse(ctx, acct, region, id)
			if err != nil {
				log.Printf("account %s region %s: fsx %s refcount check: %v — holding back", acct.label, region, id, err)
				sum.FSxSkipped++
				continue
			}
			if inUse {
				log.Printf("fsx %s in %s/%s expired (%s) but still in use by a live instance — not reaping",
					id, acct.label, region, reason)
				sum.FSxSkipped++
				continue
			}

			if r.dryRun {
				log.Printf("WOULD reap fsx %s in %s/%s — %s (deadline %s), refcount 0",
					id, acct.label, region, reason, deadline)
				r.notifyFSx(id, acct.label, region, reason, deadline, true)
				sum.FSxSkipped++
				continue
			}

			if err := r.deleteFSx(ctx, fsxClient, id); err != nil {
				log.Printf("account %s region %s: delete fsx %s: %v", acct.label, region, id, err)
				sum.Errors++
				continue
			}
			log.Printf("REAPED fsx %s in %s/%s — %s (deadline %s), refcount 0",
				id, acct.label, region, reason, deadline)
			r.notifyFSx(id, acct.label, region, reason, deadline, false)
			sum.FSxReaped++
		}
	}
}

// evaluateFSx decides whether a filesystem is past its deadline/max-age. Mirrors
// evaluate() for instances: spawn:ttl-deadline is authoritative; lacking one,
// fall back to a max-age ceiling anchored on spawn:fsx-created (or CreationTime).
// Only spawn-managed filesystems in a deletable lifecycle are considered.
func (r *reaper) evaluateFSx(fsItem fsxtypes.FileSystem, now time.Time) (id, reason, deadline string, expired bool) {
	id = aws.ToString(fsItem.FileSystemId)
	tags := fsxTagMap(fsItem.Tags)
	if tags[tagprefix.Tag("managed")] != "true" {
		return id, "", "", false
	}
	// Skip filesystems already being created/deleted/failed — only reap AVAILABLE.
	if fsItem.Lifecycle != fsxtypes.FileSystemLifecycleAvailable {
		return id, "", "", false
	}

	if dl := tags[tagprefix.Tag("ttl-deadline")]; dl != "" {
		if t, err := time.Parse(time.RFC3339, dl); err == nil {
			if now.After(t) {
				return id, "ttl-deadline", dl, true
			}
			return id, "", "", false // within deadline — honored intent
		}
		log.Printf("fsx %s: unparseable ttl-deadline %q, falling back to max-age", id, dl)
	}

	created := launchTime(tags[tagprefix.Tag("fsx-created")], fsItem.CreationTime)

	// Ephemeral-orphan safety net (#210): an ephemeral FSx carries no ttl-deadline
	// (it relies on refcount→0 reclamation on instance termination). If its launch
	// never succeeded, no instance ever references it and that reclamation never
	// fires — so once it's older than the orphan grace it's eligible regardless of
	// max-age. The refcount check in reapFSxRegion (which now also counts
	// spawn:fsx-pending) still gates the actual delete, so a legitimately-
	// provisioning filesystem inside its mount window is never reaped here.
	if tags[tagprefix.Tag("fsx-lifecycle")] == "ephemeral" {
		if !created.IsZero() && now.Sub(created) > ephemeralOrphanGrace {
			return id, "ephemeral-orphan", created.UTC().Format(time.RFC3339), true
		}
	}

	if !created.IsZero() && now.Sub(created) > r.maxAge {
		return id, "max-age", created.UTC().Format(time.RFC3339), true
	}
	return id, "", "", false
}

// fsxInUse reports whether any live instance still references the filesystem.
// Two leases count as in-use:
//   - spawn:fsx-id=<fs> — the mounted lease, written once the filesystem is
//     AVAILABLE and bound to the instance (the normal refcount).
//   - spawn:fsx-pending=<fs> — the provisioning lease, written at launch while
//     spored waits for the filesystem to reach AVAILABLE and mounts it (the ~10-
//     min window). Counting this prevents the #210 orphan safety net from reaping
//     a healthy ephemeral FSx during its legitimate provisioning window.
//
// A single live user (either lease) blocks reaping. Stopped instances count —
// they still "own" the data and may restart. Self-heals leaked leases: a gone
// instance simply isn't returned by DescribeInstances.
func (r *reaper) fsxInUse(ctx context.Context, acct account, region, fsxID string) (bool, error) {
	client := acct.ec2For(region)
	for _, leaseTag := range []string{"fsx-id", "fsx-pending"} {
		out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{
				{Name: aws.String(tagprefix.FilterTag(leaseTag)), Values: []string{fsxID}},
				{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
			},
		})
		if err != nil {
			return false, err
		}
		for _, res := range out.Reservations {
			if len(res.Instances) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// deleteFSx deletes a filesystem. It does NOT set SkipFinalExport, so an attached
// export DRA flushes remaining changes to S3 on delete — never silently dropping
// un-exported data (#184). Idempotent on already-deleting/not-found.
func (r *reaper) deleteFSx(ctx context.Context, fsxClient *fsx.Client, id string) error {
	_, err := fsxClient.DeleteFileSystem(ctx, &fsx.DeleteFileSystemInput{
		FileSystemId: aws.String(id),
	})
	if err != nil {
		if strings.Contains(err.Error(), "FileSystemNotFound") || strings.Contains(err.Error(), "BadRequest") {
			// Already gone / already deleting — idempotent success.
			return nil
		}
		return err
	}
	return nil
}

func fsxTagMap(tags []fsxtypes.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// notifyFSx posts an FSx reap (or dry-run would-reap) to REAPER_NOTIFY_URL,
// mirroring notify() for instances. Best-effort.
func (r *reaper) notifyFSx(id, account, region, reason, deadline string, dryRun bool) {
	if r.notifyURL == "" {
		return
	}
	verb := "🪓 Reaped FSx"
	if dryRun {
		verb = "🔎 [dry-run] would reap FSx"
	}
	text := fmt.Sprintf("%s `%s` in %s/%s — %s expired (deadline %s), no live users",
		verb, id, account, region, reason, deadline)
	body, _ := json.Marshal(map[string]string{"text": text})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", r.notifyURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("notifyFSx: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		log.Printf("notifyFSx: post: %v", err)
		return
	}
	_ = resp.Body.Close()
}

// tryGracefulPreStop attempts to run the instance's --pre-stop hook via SSM
// RunCommand before the reaper hard-terminates it (#187). It exists for the case
// spored is dead/wedged and never ran pre-stop itself — when spored is healthy,
// the in-instance path already flushed and this is just a (harmless) no-op the
// reaper rarely reaches.
//
// It is STRICTLY best-effort and bounded: it skips silently unless the instance
// is running, has a pre-stop hook, and the account exposes an SSM client. Any
// error (not SSM-managed, command failed, timed out) is logged and swallowed —
// the caller terminates regardless, so the hard-deadline guarantee (#72) is
// never weakened by a graceful attempt.
func (r *reaper) tryGracefulPreStop(ctx context.Context, acct account, c candidate) {
	if c.preStop == "" || !c.running || acct.ssmFor == nil {
		return
	}

	// Bound the whole attempt: min(the hook's own timeout, the reaper's hard cap).
	wait := r.gracefulWait
	if c.preStopTimeout > 0 && c.preStopTimeout < wait {
		wait = c.preStopTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	// Run as the instance's user (#63), matching the in-instance pre-stop.
	command := c.preStop
	if c.localUsername != "" {
		command = fmt.Sprintf("su - %s -c %s", c.localUsername, shellQuote(c.preStop))
	}

	ssmClient := acct.ssmFor(c.region)
	send, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{c.id},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands":         {command},
			"executionTimeout": {fmt.Sprintf("%d", int(wait.Seconds()))},
		},
	})
	if err != nil {
		// Most common: instance isn't SSM-managed (no agent / no role) →
		// InvalidInstanceId. Nothing we can do; terminate proceeds.
		log.Printf("graceful pre-stop: %s not reachable via SSM (%v) — terminating without flush", c.id, err)
		return
	}
	cmdID := aws.ToString(send.Command.CommandId)
	log.Printf("graceful pre-stop: running hook on %s (cmd %s, wait %s)", c.id, cmdID, wait)

	// Poll for terminal status, bounded by ctx.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("graceful pre-stop: %s did not finish within %s — terminating anyway", c.id, wait)
			return
		case <-ticker.C:
			out, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
				CommandId:  aws.String(cmdID),
				InstanceId: aws.String(c.id),
			})
			if err != nil {
				continue // invocation may not be registered yet; keep polling until ctx expires
			}
			switch out.Status {
			case ssmtypes.CommandInvocationStatusSuccess:
				log.Printf("graceful pre-stop: hook succeeded on %s — terminating", c.id)
				return
			case ssmtypes.CommandInvocationStatusFailed,
				ssmtypes.CommandInvocationStatusCancelled,
				ssmtypes.CommandInvocationStatusTimedOut:
				log.Printf("graceful pre-stop: hook %s on %s — terminating anyway", out.Status, c.id)
				return
			}
		}
	}
}

// shellQuote single-quotes a string for safe embedding in `su - user -c '<cmd>'`,
// escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// deleteDNS removes the Route53 records the instance registered, since spored's
// graceful DeleteDNS never ran (the reaper only fires when spored failed) (#247).
// It deletes the canonical A-record {dns-name}.{account-base36}.{domain} and, if
// the instance carried the #121 friendly-name alias, the CNAME
// {dns-name}.{account-name}.{domain}. The zone is in the reaper's own account, so
// it uses r.route53Client (base creds), NOT the per-instance cross-account role.
//
// Strictly best-effort: a disabled/zoneless reaper, an instance that registered
// no DNS, a missing record, or a Route53 error are all logged and skipped — DNS
// cleanup must never block or fail a reap (the hard-deadline guarantee, #72).
func (r *reaper) deleteDNS(ctx context.Context, c candidate) {
	if r.route53Client == nil {
		return // teardown disabled
	}
	records := dnsRecordsToDelete(c, r.dnsDomain)
	if records == nil && c.dnsName != "" && c.accountBase36 == "" {
		log.Printf("DNS teardown: %s has spawn:dns-name=%q but no spawn:account-base36 tag — skipping", c.id, c.dnsName)
		return
	}
	for _, rec := range records {
		r.deleteRecord(ctx, rec.fqdn, rec.rrType)
	}
}

// dnsRecord is a single Route53 record the reaper will delete for a candidate.
type dnsRecord struct {
	fqdn   string
	rrType r53types.RRType
}

// dnsRecordsToDelete computes the Route53 records a reaped instance registered,
// from its tags (#247) — pure, so it's unit-tested without AWS. Returns nil when
// the instance registered no DNS (no spawn:dns-name) or lacks the account-base36
// needed to build the canonical FQDN. The canonical A-record is
// {dns-name}.{account-base36}.{domain}; if the instance also carried the #121
// friendly-name alias (spawn:account-name), its CNAME
// {dns-name}.{account-name}.{domain} is included too.
func dnsRecordsToDelete(c candidate, domain string) []dnsRecord {
	if c.dnsName == "" || c.accountBase36 == "" || domain == "" {
		return nil
	}
	recs := []dnsRecord{
		{fqdn: fmt.Sprintf("%s.%s.%s", c.dnsName, c.accountBase36, domain), rrType: r53types.RRTypeA},
	}
	if c.accountName != "" {
		recs = append(recs, dnsRecord{
			fqdn:   fmt.Sprintf("%s.%s.%s", c.dnsName, c.accountName, domain),
			rrType: r53types.RRTypeCname,
		})
	}
	return recs
}

// deleteRecord deletes a single Route53 record set by name+type. It must first
// read the existing record set (Route53 DELETE requires the exact current
// rdata/TTL), so a missing record is a clean no-op. Best-effort + dry-run aware.
func (r *reaper) deleteRecord(ctx context.Context, fqdn string, rrType r53types.RRType) {
	if r.dryRun {
		log.Printf("WOULD delete DNS %s record %s", rrType, fqdn)
		return
	}
	out, err := r.route53Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(r.dnsZoneID),
		StartRecordName: aws.String(fqdn),
		StartRecordType: rrType,
		MaxItems:        aws.Int32(1),
	})
	if err != nil {
		log.Printf("DNS teardown: list %s %s failed: %v", rrType, fqdn, err)
		return
	}
	var rec *r53types.ResourceRecordSet
	for i := range out.ResourceRecordSets {
		rs := out.ResourceRecordSets[i]
		if strings.TrimSuffix(aws.ToString(rs.Name), ".") == fqdn && rs.Type == rrType {
			rec = &rs
			break
		}
	}
	if rec == nil {
		return // already gone — fine
	}
	if _, err := r.route53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(r.dnsZoneID),
		ChangeBatch: &r53types.ChangeBatch{
			Comment: aws.String("ttl-reaper: instance reaped (#247)"),
			Changes: []r53types.Change{
				{Action: r53types.ChangeActionDelete, ResourceRecordSet: rec},
			},
		},
	}); err != nil {
		log.Printf("DNS teardown: delete %s %s failed: %v", rrType, fqdn, err)
		return
	}
	log.Printf("DNS teardown: deleted %s record %s", rrType, fqdn)
}

// sweepAccountDNS reconciles the account's {base36}.{domain} A-records against the
// set of live instance public IPs and deletes any A-record whose IP has no live
// owner (#438). It's the record-driven complement to the instance-driven #247
// teardown: #247 can only delete a record while the instance is still visible via
// DescribeInstances, so a record orphaned by an abrupt exit (crash / out-of-band
// terminate / fast spot reclaim) that has since aged out of the EC2 API is
// unreachable by #247 — this catches it.
//
// Safety: strictly opt-in (REAPER_DNS_SWEEP), dry-run aware, and it aborts without
// deleting anything if the live-IP scan errored (a partial live set could delete a
// healthy record). It only touches the exact {base36}.{domain} A-records, never the
// #121 friendly CNAMEs (those alias the A-record and are torn down with it by #247;
// a CNAME carries no IP to reconcile).
func (r *reaper) sweepAccountDNS(ctx context.Context, acct account, sum *Summary) {
	if r.route53Client == nil || acct.accountID == "" {
		if acct.accountID == "" {
			log.Printf("DNS sweep: account %s has no resolved account ID — skipping", acct.label)
		}
		return
	}
	subdomain := dns.EncodeAccountID(acct.accountID) + "." + r.dnsDomain

	// Build the live public-IP set across all regions. If ANY region errors we
	// abort the sweep for this account — deleting against an incomplete live set
	// could remove a healthy instance's record.
	live := map[string]bool{}
	for _, region := range r.regions {
		ips, err := r.liveInstanceIPs(ctx, acct, region)
		if err != nil {
			log.Printf("DNS sweep: account %s region %s live-IP scan failed: %v — aborting sweep for this account", acct.label, region, err)
			return
		}
		for _, ip := range ips {
			live[ip] = true
		}
	}

	// List the account's A-records and find the orphans.
	records, err := r.listAccountARecords(ctx, subdomain)
	if err != nil {
		log.Printf("DNS sweep: account %s list records under %s failed: %v", acct.label, subdomain, err)
		return
	}
	sum.DNSScanned += len(records)
	orphans := orphanedRecords(records, live)

	for _, o := range orphans {
		if r.dryRun {
			log.Printf("DNS sweep: WOULD delete orphaned A-record %s -> %s (no live instance)", o.fqdn, o.ip)
			sum.DNSReaped++
			continue
		}
		r.deleteRecord(ctx, o.fqdn, r53types.RRTypeA)
		log.Printf("DNS sweep: deleted orphaned A-record %s -> %s (no live instance)", o.fqdn, o.ip)
		sum.DNSReaped++
	}
}

// aRecord is a Route53 A-record observed during the sweep: its FQDN and the single
// IPv4 address it points at.
type aRecord struct {
	fqdn string
	ip   string
}

// orphanedRecords returns the A-records whose IP is not in the live set — pure, so
// the reconciliation logic is unit-tested without AWS. A record with no resource
// values (shouldn't happen for an A-record) is treated as orphaned.
func orphanedRecords(records []aRecord, live map[string]bool) []aRecord {
	var orphans []aRecord
	for _, rec := range records {
		if !live[rec.ip] {
			orphans = append(orphans, rec)
		}
	}
	return orphans
}

// liveInstanceIPs returns the public IPv4 addresses of all spawn-managed instances
// in one account+region that currently HOLD an IP (running or pending). A stopped
// instance has no public IP and its record was already released by AWS, so it need
// not be represented. Errors propagate so the caller can abort the sweep.
func (r *reaper) liveInstanceIPs(ctx context.Context, acct account, region string) ([]string, error) {
	client := acct.ec2For(region)
	var ips []string
	paginator := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String(tagprefix.FilterTag("managed")), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
		},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, res := range page.Reservations {
			for _, inst := range res.Instances {
				if ip := aws.ToString(inst.PublicIpAddress); ip != "" {
					ips = append(ips, ip)
				}
			}
		}
	}
	return ips, nil
}

// listAccountARecords lists every A-record under the account's {base36}.{domain}
// subdomain, returning each record's FQDN and its (first) IP. Paginates the whole
// zone and filters by the subdomain suffix — Route53 has no per-subdomain list, so
// we walk and match. Best-effort per record: an A-record with no values is skipped.
func (r *reaper) listAccountARecords(ctx context.Context, subdomain string) ([]aRecord, error) {
	var out []aRecord
	suffix := "." + subdomain
	input := &route53.ListResourceRecordSetsInput{HostedZoneId: aws.String(r.dnsZoneID)}
	for {
		page, err := r.route53Client.ListResourceRecordSets(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("list records: %w", err)
		}
		for i := range page.ResourceRecordSets {
			rs := page.ResourceRecordSets[i]
			if rs.Type != r53types.RRTypeA {
				continue
			}
			name := strings.TrimSuffix(aws.ToString(rs.Name), ".")
			// Match records directly under the account subdomain (name.base36.domain),
			// not the bare subdomain apex itself.
			if !strings.HasSuffix(name, suffix) || name == subdomain {
				continue
			}
			// Skip Route53 alias A-records (no literal ResourceRecords) — the sweep
			// only reconciles plain A-records that carry an IP.
			if len(rs.ResourceRecords) == 0 {
				continue
			}
			out = append(out, aRecord{fqdn: name, ip: aws.ToString(rs.ResourceRecords[0].Value)})
		}
		if page.IsTruncated {
			input.StartRecordName = page.NextRecordName
			input.StartRecordType = page.NextRecordType
			input.StartRecordIdentifier = page.NextRecordIdentifier
			continue
		}
		break
	}
	return out, nil
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
