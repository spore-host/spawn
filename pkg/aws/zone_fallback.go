package aws

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	smithy "github.com/aws/smithy-go"
)

// capacityErrorCodes are the RunInstances error codes that mean "no capacity
// for this request here", which another placement may not share. A job array
// or pool advances its fallback chain on them (taskcohort.Classifier), and a
// single launch tries the other zones that offer the type on all but the
// regional Spot limit ([IsZoneCapacityErrorCode], #1458). Everything
// else (a bad AMI, a denied action, a quota) fails the same way in every zone.
var capacityErrorCodes = map[string]bool{
	"InsufficientInstanceCapacity": true,
	"InsufficientHostCapacity":     true,
	"MaxSpotInstanceCountExceeded": true,
	"SpotMaxPriceTooLow":           true,
}

// IsCapacityErrorCode reports whether an AWS error code from RunInstances is a
// capacity error: one another placement (zone, rung) might not hit.
func IsCapacityErrorCode(code string) bool { return capacityErrorCodes[code] }

// IsZoneCapacityErrorCode reports whether a capacity error is one zone's, so
// another zone may not share it: every capacity code but
// MaxSpotInstanceCountExceeded, the region's Spot instance limit, which every
// zone of the region hits alike. A single launch moves zone only on these
// (#1458); the cohort classifiers still count the Spot limit as capacity.
func IsZoneCapacityErrorCode(code string) bool {
	return IsCapacityErrorCode(code) && code != regionalSpotLimitCode
}

// regionalSpotLimitCode is the Spot instance limit of a whole region.
const regionalSpotLimitCode = "MaxSpotInstanceCountExceeded"

// regionalSpotLimitError is a launch refused for the region's Spot instance
// limit: no zone would do better, so none other is tried or named.
func regionalSpotLimitError(err error, region string) error {
	return fmt.Errorf("%w\n%s is %s's Spot instance limit, not one zone's capacity, so spawn did not try another "+
		"availability zone: every zone in the region would refuse it the same way. Wait for Spot instances to end, "+
		"raise the Spot quota, or launch on-demand (without --spot)", err, regionalSpotLimitCode, region)
}

// ZoneAttempt is one RunInstances attempt of a launch that can move zone on a
// capacity error (#1458). Zone "" is the first attempt of a launch that named
// no zone, which EC2 placed in a default subnet of its choosing.
type ZoneAttempt struct {
	Zone    string `json:"zone"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// label names the attempt's zone for a person.
func (a ZoneAttempt) label() string {
	if a.Zone == "" {
		return "EC2's choice of zone (none named)"
	}
	return a.Zone
}

// ZoneOfferings are the availability zones of a region that offer an
// instance type (DescribeInstanceTypeOfferings, location type
// availability-zone), and of those the ones a launch naming no subnet can
// place in: zones with a default subnet. Both sorted.
type ZoneOfferings struct {
	Offered    []string
	Launchable []string
}

// zoneOfferingsAPI is the EC2 a zone lookup needs. *ec2.Client satisfies it.
type zoneOfferingsAPI interface {
	DescribeInstanceTypeOfferings(ctx context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
}

// InstanceTypeZones lists the zones of region that offer instanceType, and
// those with a default subnet (#1458). A job array's AZ chain and a single
// launch's zone fallback both come from it, so neither tries a zone that does
// not offer the type, or one a default-VPC launch cannot place in.
func (c *Client) InstanceTypeZones(ctx context.Context, region, instanceType string) (ZoneOfferings, error) {
	return instanceTypeZones(ctx, c.regionalEC2(region), instanceType)
}

func instanceTypeZones(ctx context.Context, api zoneOfferingsAPI, instanceType string) (ZoneOfferings, error) {
	offered := map[string]bool{}
	offerIn := &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: types.LocationTypeAvailabilityZone,
		Filters:      []types.Filter{{Name: aws.String("instance-type"), Values: []string{instanceType}}},
	}
	for {
		out, err := api.DescribeInstanceTypeOfferings(ctx, offerIn)
		if err != nil {
			return ZoneOfferings{}, fmt.Errorf("the zones offering %s (ec2:DescribeInstanceTypeOfferings): %w", instanceType, err)
		}
		for _, o := range out.InstanceTypeOfferings {
			if string(o.InstanceType) == instanceType && aws.ToString(o.Location) != "" {
				offered[aws.ToString(o.Location)] = true
			}
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		offerIn.NextToken = out.NextToken
	}

	defaults := map[string]bool{}
	subnetIn := &ec2.DescribeSubnetsInput{
		Filters: []types.Filter{{Name: aws.String("default-for-az"), Values: []string{"true"}}},
	}
	for {
		out, err := api.DescribeSubnets(ctx, subnetIn)
		if err != nil {
			return ZoneOfferings{Offered: sortedZones(offered)}, fmt.Errorf("the zones with a default subnet (ec2:DescribeSubnets): %w", err)
		}
		for _, sn := range out.Subnets {
			// default-for-az is the filter; the flag is checked too, so a
			// subnet answered outside it is not trusted.
			if aws.ToBool(sn.DefaultForAz) && aws.ToString(sn.AvailabilityZone) != "" {
				defaults[aws.ToString(sn.AvailabilityZone)] = true
			}
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		subnetIn.NextToken = out.NextToken
	}

	z := ZoneOfferings{Offered: sortedZones(offered)}
	for _, zone := range z.Offered {
		if defaults[zone] {
			z.Launchable = append(z.Launchable, zone)
		}
	}
	return z, nil
}

func sortedZones(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ZoneFallbackBlocker says why a launch must stay in the one zone it would
// first be placed in, or "" when a capacity error may move it to another
// zone (#1458). Each reason is something that exists in, or was checked for,
// one zone or one subnet:
//
//   - an AZ, given with --az, piped from truffle, or chosen with --vpc;
//   - a subnet (--subnet-id), which is in exactly one zone;
//   - a capacity reservation or Capacity Block, which is in one zone;
//   - a cluster placement group, which is in one zone;
//   - an FSx for Lustre filesystem that already exists (--fsx-id,
//     --fsx-recall, or a durable --fsx-create), in one zone's subnet, which
//     spawn mounts only from that zone (#208);
//   - --no-public-ip, whose endpoint and route check is of one subnet (#1433);
//   - more than one EFA interface, laid out in one subnet (#1410).
//
// The last two need a subnet or an AZ anyway (ValidateNoPublicIP,
// ValidateEFASubnet), so the first two reasons catch them first; they are
// here so a caller that skipped those checks still cannot move them.
//
// Not reasons: EFS (regional, with a mount target per zone; the boot-time
// fallback IP of a launch naming no zone is any zone's already), EBS volumes
// from --attach-volume (created from a regional snapshot in whatever zone the
// instance lands in), and an ephemeral --fsx-create, which is created after
// the launch, in the zone the instance landed in.
func (cfg LaunchConfig) ZoneFallbackBlocker() string {
	switch {
	case cfg.AvailabilityZone != "":
		return fmt.Sprintf("the launch is pinned to %s (--az)", cfg.AvailabilityZone)
	case cfg.SubnetID != "":
		return fmt.Sprintf("the launch is pinned to subnet %s, which is in one zone (--subnet-id)", cfg.SubnetID)
	case cfg.ReservationID != "" && cfg.CapacityBlock:
		return fmt.Sprintf("Capacity Block %s is in one zone (--reservation-id)", cfg.ReservationID)
	case cfg.ReservationID != "":
		return fmt.Sprintf("capacity reservation %s is in one zone (--reservation-id)", cfg.ReservationID)
	case cfg.PlacementGroup != "":
		return fmt.Sprintf("placement group %s is in one zone (--placement-group)", cfg.PlacementGroup)
	case cfg.FSxLustreID != "":
		return fmt.Sprintf("FSx for Lustre filesystem %s is in one zone, and an instance in another could not mount it", cfg.FSxLustreID)
	case cfg.NoPublicIP:
		return "--no-public-ip checked the endpoints and routes of one subnet"
	case cfg.EFAEnabled && cfg.EFAInterfaces > 1:
		return fmt.Sprintf("the %d EFA interfaces are laid out in one subnet", cfg.EFAInterfaces)
	}
	return ""
}

// ZoneFallbackError is a launch that hit a capacity error and either tried
// the other zones offering its instance type and launched in none, or could
// not move (Blocker). Unwrap gives the last attempt's *LaunchError, so its
// AWS code still classifies the failure.
type ZoneFallbackError struct {
	InstanceType, Region string
	// Attempts are the RunInstances attempts, in order, each with its code.
	Attempts []ZoneAttempt
	// Blocker is why no other zone was tried ("" when they were).
	Blocker string
	// OtherZones offer the type and were not tried: where a pinned launch
	// could go instead.
	OtherZones []string
	// LookupErr is the zone lookup's failure, when it failed.
	LookupErr error
	err       error
}

func (e *ZoneFallbackError) Unwrap() error { return e.err }

func (e *ZoneFallbackError) Error() string {
	var b strings.Builder
	last := e.Attempts[len(e.Attempts)-1]
	capacity := IsZoneCapacityErrorCode(last.Code)
	switch {
	case e.Blocker != "":
		where := ""
		if last.Zone != "" {
			where = " in " + last.Zone
		}
		fmt.Fprintf(&b, "failed to launch instance: %s%s: %s\n", last.Code, where, last.Message)
		fmt.Fprintf(&b, "spawn did not try another availability zone: %s.", e.Blocker)
		if len(e.OtherZones) > 0 {
			fmt.Fprintf(&b, " %s is also offered in %s; name one with --az.", e.InstanceType, strings.Join(e.OtherZones, ", "))
		}
	case len(e.Attempts) == 1:
		fmt.Fprintf(&b, "failed to launch instance: %s in %s: %s\n", last.Code, last.label(), last.Message)
		if e.LookupErr != nil {
			fmt.Fprintf(&b, "spawn could not list the other zones to try: %v.", e.LookupErr)
		} else {
			fmt.Fprintf(&b, "No other zone in %s both offers %s and has a default subnet to launch in.", e.Region, e.InstanceType)
		}
	default:
		if capacity {
			fmt.Fprintf(&b, "failed to launch instance: no capacity for %s in %s in any zone tried (%d attempts):\n", e.InstanceType, e.Region, len(e.Attempts))
		} else {
			fmt.Fprintf(&b, "failed to launch instance: %s in %s, after capacity errors elsewhere; not trying further zones:\n", last.Code, last.label())
		}
		for _, a := range e.Attempts {
			fmt.Fprintf(&b, "  - %s: %s: %s\n", a.label(), a.Code, a.Message)
		}
	}
	msg := strings.TrimRight(b.String(), "\n")
	b.Reset()
	b.WriteString(msg)
	if capacity {
		var maxAttempts *retry.MaxAttemptsError
		if errors.As(e.err, &maxAttempts) {
			fmt.Fprintf(&b, "\n(\"exceeded maximum number of attempts\" is the AWS SDK re-sending the same request to the same zone; it never tries another.)")
		}
		fmt.Fprintf(&b, "\nTo wait for capacity: lagotto watch %s --regions %s --action notify", e.InstanceType, e.Region)
	}
	return strings.TrimRight(b.String(), "\n")
}

// zoneLaunchAPI is the EC2 a zone-fallback launch calls: RunInstances and the
// zone lookup. Nothing in it creates anything but the instance.
type zoneLaunchAPI interface {
	runInstancesAPI
	zoneOfferingsAPI
}

// LaunchWithZoneFallback is Launch, except that a launch naming no zone (and
// nothing else bound to one, [LaunchConfig.ZoneFallbackBlocker]) whose
// RunInstances fails on capacity tries the other zones that offer its type
// and have a default subnet, in order, until one launches (#1458). Progress
// goes to out. Everything Launch creates before RunInstances (the launch
// config object, tags, the AMI lookup, the private-network check) is done
// once; only RunInstances is repeated, each attempt with its own ClientToken.
// A pinned launch that fails on capacity is not moved; its error names the
// other zones offering the type. The result's ZoneAttempts are the attempts
// that failed before the one that launched.
func (c *Client) LaunchWithZoneFallback(ctx context.Context, launchConfig LaunchConfig, out io.Writer) (*LaunchResult, error) {
	p, err := c.prepareLaunch(ctx, launchConfig)
	if err != nil {
		return nil, err
	}
	result, attempts, err := launchAcrossZones(ctx, p.ec2, p, out)
	if err != nil {
		return nil, err
	}
	launched, err := c.completeLaunch(ctx, p, result)
	if launched != nil {
		launched.ZoneAttempts = attempts
	}
	return launched, err
}

// launchAcrossZones sends p's RunInstances, then, on a capacity error and
// with nothing binding the launch to its zone, the same input with a zone and
// a new ClientToken for each other launchable zone offering the type. A
// capacity error left nothing behind (EC2 launched nothing), so the next
// attempt starts clean; any other error stops the walk. On success p's input
// and config carry the zone that launched.
func launchAcrossZones(ctx context.Context, api zoneLaunchAPI, p *preparedLaunch, out io.Writer) (*ec2.RunInstancesOutput, []ZoneAttempt, error) {
	cfg := &p.cfg
	blocker := cfg.ZoneFallbackBlocker()
	base := ""
	if blocker == "" {
		// Each attempt is a different request, so it gets its own token: one
		// token on two placements is IdempotentParameterMismatch. Set here,
		// not left to the SDK, which fills a nil token into the input it is
		// given, so a copy of it would carry the first attempt's.
		base = cfg.ClientToken
		if base == "" {
			base = "spawn-" + rand.Text()
		}
		p.input.ClientToken = aws.String(base)
	}
	result, err := runInstancesWithRetry(ctx, api, p.input)
	if err == nil {
		return result, nil, nil
	}
	code, msg := awsErrorDetail(err)
	if code == regionalSpotLimitCode {
		return nil, nil, regionalSpotLimitError(newLaunchError(err), cfg.Region)
	}
	if !IsZoneCapacityErrorCode(code) {
		return nil, nil, newLaunchError(err)
	}
	attempts := []ZoneAttempt{{Zone: cfg.AvailabilityZone, Code: code, Message: msg}}
	zones, lerr := instanceTypeZones(ctx, api, cfg.InstanceType)
	fail := &ZoneFallbackError{InstanceType: cfg.InstanceType, Region: cfg.Region, Attempts: attempts, Blocker: blocker, LookupErr: lerr, err: newLaunchError(err)}
	if blocker != "" {
		fail.OtherZones = otherZones(zones.Offered, cfg.AvailabilityZone)
		return nil, nil, fail
	}
	if lerr != nil || len(zones.Launchable) == 0 {
		return nil, nil, fail
	}

	fmt.Fprintf(out, "   %s in %s; trying the zones that offer %s: %s\n", code, attempts[0].label(), cfg.InstanceType, strings.Join(zones.Launchable, ", "))
	for i, zone := range zones.Launchable {
		in := *p.input
		placement := types.Placement{}
		if p.input.Placement != nil {
			placement = *p.input.Placement
		}
		placement.AvailabilityZone = aws.String(zone)
		in.Placement = &placement
		in.ClientToken = aws.String(zoneAttemptToken(base, i+1))
		result, err := runInstancesWithRetry(ctx, api, &in)
		if err == nil {
			fmt.Fprintf(out, "   launched in %s\n", zone)
			cfg.AvailabilityZone = zone
			p.input = &in
			return result, attempts, nil
		}
		code, msg := awsErrorDetail(err)
		attempts = append(attempts, ZoneAttempt{Zone: zone, Code: code, Message: msg})
		fmt.Fprintf(out, "   %s: %s\n", zone, code)
		fail.Attempts, fail.err = attempts, newLaunchError(err)
		if code == regionalSpotLimitCode {
			return nil, nil, regionalSpotLimitError(newLaunchError(err), cfg.Region)
		}
		if !IsZoneCapacityErrorCode(code) {
			break
		}
	}
	return nil, nil, fail
}

// zoneAttemptToken is attempt n's ClientToken: base with the attempt number,
// within RunInstances' 64-character limit.
func zoneAttemptToken(base string, n int) string {
	suffix := "-z" + strconv.Itoa(n)
	if len(base)+len(suffix) > 64 {
		sum := sha256.Sum256([]byte(base))
		base = "spawn-" + hex.EncodeToString(sum[:])[:40]
	}
	return base + suffix
}

// awsErrorDetail is err's AWS error code and AWS's own message for it.
func awsErrorDetail(err error) (code, msg string) {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code, msg = apiErr.ErrorCode(), apiErr.ErrorMessage()
	}
	if msg == "" {
		msg = err.Error()
	}
	return code, msg
}

func otherZones(zones []string, drop string) []string {
	out := make([]string, 0, len(zones))
	for _, z := range zones {
		if z != drop {
			out = append(out, z)
		}
	}
	return out
}
