package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/sweep"
)

// --no-public-ip on a detached resume (#1468).
//
// A detached sweep's --no-public-ip members had their subnets checked at
// --detach (detachedSubnetCheck) and pinned in their launch settings. The
// orchestrator cannot repeat the check (it needs VPC reads its role has not
// got), and `spawn sweep resume --detach` only re-invokes it, so a VPC
// endpoint or NAT route removed since the detach would go unnoticed: the
// remaining members would launch where spored cannot enforce their TTL or cost
// limit. So the resume re-runs the check, with the caller's credentials, for
// the subnets the orchestrator will launch into, before re-invoking it. The
// params are read, never rewritten: the orchestrator still launches what was
// uploaded.
//
// The override is resume's own --allow-missing-endpoints: the uploaded
// settings do not record the launch's, and an override given at --detach was
// given for the subnets as they were then.

// resumeAllowMissingEndpoints is resume's --allow-missing-endpoints.
var resumeAllowMissingEndpoints bool

// downloadDetachedParams reads a detached sweep's uploaded params. A seam so
// tests need no AWS.
var downloadDetachedParams = func(ctx context.Context, infra awssdk.Config, rec *sweep.SweepRecord) (*ParamFileFormat, error) {
	pf, err := sweep.DownloadParamsFromS3(ctx, infra, rec.S3ParamsKey)
	if err != nil {
		return nil, err
	}
	return &ParamFileFormat{Defaults: pf.Defaults, Params: pf.Params}, nil
}

// detachedSubnetRecheck is one subnet check a detached resume makes: the
// first pending member (index) whose settings pin that region, subnet and
// need set.
type detachedSubnetRecheck struct {
	index int
	cfg   aws.LaunchConfig
}

// detachedResumePending is the parameter sets the orchestrator will launch
// when re-invoked: each region's queue on a multi-region sweep, else every
// set from NextToLaunch on (lambda/sweep-orchestrator's two paths).
func detachedResumePending(rec *sweep.SweepRecord) []int {
	var out []int
	if rec.MultiRegion && len(rec.RegionStatus) > 0 {
		seen := map[int]bool{}
		for _, rs := range rec.RegionStatus {
			if rs == nil {
				continue
			}
			for _, i := range rs.NextToLaunch {
				if !seen[i] {
					seen[i] = true
					out = append(out, i)
				}
			}
		}
		sort.Ints(out)
		return out
	}
	for i := rec.NextToLaunch; i < rec.TotalParams; i++ {
		out = append(out, i)
	}
	return out
}

// detachedSubnetRechecks is the subnet checks resuming rec needs: one per
// distinct region, pinned subnet and need set among the pending members whose
// uploaded launch settings carry no_public_ip. Pure: params only.
//
// Each check's config is rebuilt from what the params carry and the check
// reads: the region, the pinned subnet and AZ, the member's tags (a burst
// member needs DynamoDB) and its target OS from them (a Windows bootstrap
// needs NAT). Settings with no_public_ip and no subnet were not written by
// the CLI, which pins one, and are refused.
func detachedSubnetRechecks(rec *sweep.SweepRecord, pf *ParamFileFormat) ([]detachedSubnetRecheck, error) {
	var out []detachedSubnetRecheck
	seen := map[string]bool{}
	for _, idx := range detachedResumePending(rec) {
		if idx < 0 || idx >= len(pf.Params) {
			return nil, fmt.Errorf("the sweep's record has parameter set %d pending, but its uploaded params have %d sets", idx, len(pf.Params))
		}
		row := pf.Params[idx]
		raw, ok := row[detachedLaunchSettingsKey]
		if !ok {
			continue // params from a CLI that wrote no settings: no --no-public-ip
		}
		s, err := aws.DecodeLaunchSettings(raw)
		if err != nil {
			return nil, fmt.Errorf("parameter set %d: %w", idx, err)
		}
		if !s.NoPublicIP {
			continue
		}
		if s.SubnetID == "" {
			return nil, fmt.Errorf("parameter set %d: its launch settings ask for no public IP but pin no subnet, "+
				"so there is no subnet to check (the orchestrator refuses them too)", idx)
		}
		region := paramSetRegion(pf, idx)
		if region == "" {
			return nil, fmt.Errorf("parameter set %d: its uploaded params name no region, so its subnet %s cannot be checked", idx, s.SubnetID)
		}
		tags := map[string]string{}
		if m, ok := row[detachedTagsKey].(map[string]interface{}); ok {
			for k, v := range m {
				if sv, ok := v.(string); ok {
					tags[k] = sv
				}
			}
		}
		cfg := aws.LaunchConfig{
			Region: region, SubnetID: s.SubnetID, AvailabilityZone: s.AvailabilityZone,
			NoPublicIP: true, Tags: tags,
			// The tag aws.buildTags writes from LaunchConfig.TargetOS.
			TargetOS: tags["spawn:os"],
		}
		key := fmt.Sprintf("%s|%s|%v", region, s.SubnetID, privateNeedsKey(cfg))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, detachedSubnetRecheck{index: idx, cfg: cfg})
	}
	return out, nil
}

// privateNeedsKey is what, besides the subnet, decides a rebuilt config's
// check: two members that agree on it get the same answer.
func privateNeedsKey(cfg aws.LaunchConfig) []bool {
	var key []bool
	for _, s := range aws.PrivateServices() {
		key = append(key, s.AppliesTo(cfg))
	}
	return append(key, cfg.TargetOS == "windows")
}

// recheckDetachedNoPublicIP re-runs --no-public-ip's subnet check for every
// subnet the orchestrator will launch a pending private-only member into,
// with the caller's credentials (newClient), before a detached resume
// re-invokes it (#1468). allow is resume's --allow-missing-endpoints. A sweep
// with no such member makes no AWS call here, and allow is refused for it, as
// a launch refuses --allow-missing-endpoints without --no-public-ip.
func recheckDetachedNoPublicIP(ctx context.Context, rec *sweep.SweepRecord, pf *ParamFileFormat, allow bool,
	newClient func(context.Context) (*aws.Client, error), out io.Writer) error {
	checks, err := detachedSubnetRechecks(rec, pf)
	if err != nil {
		return err
	}
	if len(checks) == 0 {
		if allow {
			return errors.New("--allow-missing-endpoints applies only to a sweep whose pending members were launched " +
				"with --no-public-ip, and none of this sweep's were")
		}
		return nil
	}
	client, err := newClient(ctx)
	if err != nil {
		return fmt.Errorf("--no-public-ip: re-check the pinned subnets: %w", err)
	}
	fmt.Fprintf(out, "🔍 --no-public-ip: re-checking %d pinned subnet check(s) before the orchestrator launches into them...\n", len(checks))
	for _, c := range checks {
		cfg := c.cfg
		cfg.AllowMissingEndpoints = allow
		if _, err := preflightNoPublicIPFor(ctx, client, &cfg, nil, out); err != nil {
			return fmt.Errorf("parameter set %d: %w", c.index, err)
		}
	}
	return nil
}
