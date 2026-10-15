package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/spotrequest"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// spotRequestAPI is the slice of EC2 the orphaned-Spot-request sweep needs
// (#950). Separate from ec2API for the same reason as netResourceAPI: the
// instance scan's interface, and every fake implementing it, stays untouched.
type spotRequestAPI interface {
	DescribeSpotInstanceRequests(ctx context.Context, in *ec2.DescribeSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	CancelSpotInstanceRequests(ctx context.Context, in *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// The three states of the orphaned-Spot-request sweep, mirroring
// REAPER_NET_RESOURCES and for the same reason: production runs DryRun=false,
// so a boolean would arm a new destructive sweep the moment the Lambda is
// updated. 'report' (the default) logs what it would cancel and terminate.
const (
	spotRequestsOff    = "off"
	spotRequestsReport = "report"
	spotRequestsReap   = "reap"
)

// parseSpotRequests maps REAPER_SPOT_REQUESTS onto a mode. An unrecognised
// value is 'report': a typo must neither arm the sweep nor silently disable it.
func parseSpotRequests(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case spotRequestsOff:
		return spotRequestsOff
	case spotRequestsReap:
		return spotRequestsReap
	default:
		return spotRequestsReport
	}
}

// spotReportOnly reports whether this cycle may cancel and terminate. DryRun
// wins, as for every other sweep.
func (r *reaper) spotReportOnly() bool {
	return r.dryRun || r.spotRequests != spotRequestsReap
}

// spotOpenGrace is how long a request may sit open with no instance before the
// sweep treats it as orphaned. RunInstances creates the request already
// fulfilled, so an open request with no instance is one whose instance was
// terminated. The grace absorbs any lag between the two being visible.
const spotOpenGrace = 15 * time.Minute

// Why a request is cancelled. These are log and notification text.
const (
	spotReasonNoInstance  = "no-instance"       // open, nothing launched (its instance was terminated)
	spotReasonGone        = "instance-gone"     // its instance is terminated, or no longer exists
	spotReasonReplacement = "replacement"       // its current instance is not the one spawn launched
	spotReasonExpired     = "instance-expired"  // its (original, managed) instance is past deadline/max-age
	spotReasonStranded    = "replacement-alive" // cancelled already, but its replacement is still running
)

// spotDecision is what the sweep does with one persistent request.
type spotDecision struct {
	cancel    bool   // cancel the request
	terminate string // instance to terminate: a replacement EC2 launched from a leaked request
	reason    string
}

// evaluateSpotRequest decides what to do with one spawn-tagged persistent Spot
// request. inst is the request's current instance as DescribeInstances returned
// it, or nil if the request has none or EC2 no longer knows it.
//
// The request's own spawn:ttl-deadline tag is deliberately NOT a reason to
// cancel while its original instance is alive. The tag is copied from the
// instance at launch and `spawn extend` updates only the instance, so after an
// extend it is stale. Cancelling a live instance's request on a stale deadline
// would strand it: EC2 restarts a stopped Spot instance only through its
// request. The instance's own tags are authoritative, and the instance scan
// (evaluate) reads them. That scan cancels the request before terminating, so a
// request whose live original has expired is normally already gone by now. The
// expired branch below covers a request it missed.
//
// Once the original is gone, the request has nothing left to protect and is
// cancelled whatever its deadline says.
func (r *reaper) evaluateSpotRequest(req ec2types.SpotInstanceRequest, inst *ec2types.Instance, region string, now time.Time) spotDecision {
	tags := tagMap(req.Tags)
	instID := aws.ToString(req.InstanceId)
	cancelled := req.State == ec2types.SpotInstanceStateCancelled

	live := inst != nil && inst.State != nil &&
		inst.State.Name != ec2types.InstanceStateNameTerminated &&
		inst.State.Name != ec2types.InstanceStateNameShuttingDown

	if !live {
		if cancelled {
			return spotDecision{} // cancelled, and nothing is running: done
		}
		if instID == "" {
			// Open, nothing launched. Give a just-created request its grace.
			if req.CreateTime != nil && now.Sub(*req.CreateTime) < spotOpenGrace {
				return spotDecision{}
			}
			return spotDecision{cancel: true, reason: spotReasonNoInstance}
		}
		return spotDecision{cancel: true, reason: spotReasonGone}
	}

	// A replacement: EC2 launched it from the request after the instance spawn
	// launched was terminated outside spawn. Two independent signs. Either
	// suffices:
	//   - the request names a different original (spotrequest.OriginalInstanceTag,
	//     written after RunInstances); this catches a replacement even if it
	//     inherited the tags;
	//   - the instance does not carry spawn:managed=true. Every instance spawn
	//     launches from a persistent request is tagged at RunInstances, and EC2
	//     does not copy RunInstances' instance TagSpecifications into a request's
	//     launch specification (LaunchSpecification has no tag field), so a
	//     replacement is expected to be untagged. It is then invisible to
	//     `spawn list` and to the instance scan.
	itags := tagMap(inst.Tags)
	original := tags[tagprefix.Tag("original-instance-id")] // spotrequest.OriginalInstanceTag
	if (original != "" && original != instID) || itags[tagprefix.Tag("managed")] != "true" {
		if cancelled {
			return spotDecision{terminate: instID, reason: spotReasonStranded}
		}
		return spotDecision{cancel: true, terminate: instID, reason: spotReasonReplacement}
	}

	if cancelled {
		return spotDecision{} // the instance scan owns a live, managed instance
	}
	if _, expired := r.evaluate(*inst, region, now); expired {
		return spotDecision{cancel: true, reason: spotReasonExpired}
	}
	return spotDecision{}
}

// reapSpotRequestsRegion is the orphaned-persistent-Spot-request sweep (#950).
//
// #922 made a --spot launch with a stop-based lifecycle a persistent request,
// and every spawn terminate path cancels it first. Some terminates bypass
// spawn: the EC2 console, `aws ec2 terminate-instances`, or a cancel that was
// refused before the role carried the grant. The request then reopens and
// EC2 launches a replacement, with no tags and nothing to reap it, and keeps
// doing so every time that one is terminated. This sweep finds spawn-tagged
// persistent requests and cancels the ones whose instance is gone, replaced or
// expired. It terminates a replacement after cancelling its request, in that
// order, or the terminate would reopen the request.
//
// Terminating an untagged replacement is outside the tag-scoped
// ec2:TerminateInstances grant, so it is expected to be refused. The sweep
// still cancels the request (which is spawn-tagged and therefore granted), so
// nothing further launches. It reports the replacement under
// sentinelSpotReplacementStranded with the command to end it, every run until
// someone does.
func (r *reaper) reapSpotRequestsRegion(ctx context.Context, acct account, region string, now time.Time, sum *Summary, outcome *accountOutcome) {
	if acct.spotFor == nil || r.spotRequests == spotRequestsOff {
		return
	}
	cli := acct.spotFor(region)

	var reqs []ec2types.SpotInstanceRequest
	pager := ec2.NewDescribeSpotInstanceRequestsPaginator(cli, &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String(tagprefix.FilterTag("managed")), Values: []string{"true"}},
			{Name: aws.String("type"), Values: []string{string(ec2types.SpotInstanceTypePersistent)}},
			// cancelled is included so a replacement whose request we already
			// cancelled stays visible while it runs (EC2 keeps a cancelled
			// request describable until its instance is terminated).
			{Name: aws.String("state"), Values: []string{"open", "active", "disabled", "cancelled"}},
		},
	})
	first := true
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if first {
			outcome.recordSpot(err)
			first = false
		}
		if err != nil {
			log.Printf("spot-requests: %s/%s: describe spot instance requests (%s): %v",
				acct.label, region, classifyScanError(err), err)
			if classifyScanError(err) != failureDenied {
				sum.Errors++
			}
			return
		}
		reqs = append(reqs, page.SpotInstanceRequests...)
	}
	if len(reqs) == 0 {
		return
	}
	sum.SpotRequestsScanned += len(reqs)

	insts, err := describeInstancesByID(ctx, cli, reqs)
	if err != nil {
		// Without the instances every request looks orphaned. Act on none.
		log.Printf("spot-requests: %s/%s: describe the requests' instances: %v — skipping the sweep this cycle",
			acct.label, region, err)
		sum.Errors++
		return
	}

	reportOnly := r.spotReportOnly()
	for _, req := range reqs {
		reqID := aws.ToString(req.SpotInstanceRequestId)
		var inst *ec2types.Instance
		if v, ok := insts[aws.ToString(req.InstanceId)]; ok {
			inst = &v
		}
		d := r.evaluateSpotRequest(req, inst, region, now)
		if !d.cancel && d.terminate == "" {
			continue
		}

		if reportOnly {
			log.Printf("WOULD cancel orphaned spot request %s in %s/%s (%s, instance %q)%s",
				reqID, acct.label, region, d.reason, aws.ToString(req.InstanceId),
				map[bool]string{true: " and terminate replacement " + d.terminate, false: ""}[d.terminate != ""])
			r.notifySpot(reqID, d, acct.label, region, true)
			sum.SpotRequestsSkipped++
			continue
		}

		if d.cancel {
			if err := spotrequest.Cancel(ctx, cli, reqID); err != nil {
				log.Printf("spot-requests: %s/%s: cancel %s (%s): %v", acct.label, region, reqID, d.reason, err)
				sum.Errors++
				// Never terminate with the request still open: the terminate would
				// reopen it and EC2 would launch yet another replacement.
				continue
			}
			log.Printf("CANCELLED orphaned spot request %s in %s/%s (%s, instance %q)",
				reqID, acct.label, region, d.reason, aws.ToString(req.InstanceId))
			sum.SpotRequestsCancelled++
		}

		if d.terminate != "" {
			_, err := cli.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{d.terminate}})
			if err != nil {
				if classifyScanError(err) != failureDenied {
					sum.Errors++
				}
				sum.SpotReplacementsStranded++
				log.Printf("%s: %s/%s instance %s was launched by spawn's persistent spot request %s after its "+
					"original instance was terminated outside spawn. The request is cancelled so nothing further "+
					"launches, but the reaper could not terminate the instance (%v). It is untagged, so it is invisible "+
					"to spawn list and to the reaper's tag-scoped grant, and it bills until someone runs: "+
					"aws ec2 terminate-instances --region %s --instance-ids %s",
					sentinelSpotReplacementStranded, acct.label, region, d.terminate, reqID, err, region, d.terminate)
				continue
			}
			log.Printf("TERMINATED spot replacement %s of request %s in %s/%s", d.terminate, reqID, acct.label, region)
			sum.SpotReplacementsTerminated++
		}
		r.notifySpot(reqID, d, acct.label, region, false)
	}
}

// describeInstancesByID returns the requests' current instances, keyed by ID.
// It filters on instance-id rather than passing InstanceIds, because
// InstanceIds fails the whole call on one ID EC2 no longer knows. A missing
// instance is exactly the case this sweep exists to find.
func describeInstancesByID(ctx context.Context, cli spotRequestAPI, reqs []ec2types.SpotInstanceRequest) (map[string]ec2types.Instance, error) {
	var ids []string
	for _, req := range reqs {
		if id := aws.ToString(req.InstanceId); id != "" {
			ids = append(ids, id)
		}
	}
	out := map[string]ec2types.Instance{}
	const chunk = 100 // well under EC2's per-filter value limit
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		pager := ec2.NewDescribeInstancesPaginator(cli, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: ids[start:end]}},
		})
		for pager.HasMorePages() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, res := range page.Reservations {
				for _, inst := range res.Instances {
					out[aws.ToString(inst.InstanceId)] = inst
				}
			}
		}
	}
	return out, nil
}

// notifySpot posts a sweep action to REAPER_NOTIFY_URL, like every other reap.
func (r *reaper) notifySpot(reqID string, d spotDecision, account, region string, dryRun bool) {
	verb := "🪓 Cancelled"
	if dryRun {
		verb = "🔎 [dry-run] would cancel"
	}
	text := fmt.Sprintf("%s orphaned persistent spot request `%s` in %s/%s — %s", verb, reqID, account, region, d.reason)
	if d.terminate != "" {
		text += fmt.Sprintf(", and terminate replacement `%s`", d.terminate)
	}
	if r.notifyURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, "POST", r.notifyURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("notifySpot: build request: %v", err)
		return
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(hreq)
	if err != nil {
		log.Printf("notifySpot: post: %v", err)
		return
	}
	_ = resp.Body.Close()
}
