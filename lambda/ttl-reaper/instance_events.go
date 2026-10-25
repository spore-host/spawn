package main

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"slices"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/elasticip"
)

// The event-driven launch Elastic IP release (#1287).
//
// In a nondefault VPC, ReleaseAddress on an associated address returns
// InvalidIPAddress.InUse ("[Nondefault VPC] You must disassociate the Elastic IP
// address before you can release it", EC2 API reference). On a multi-interface
// EFA instance the launch Elastic IP is the instance's only egress, because EC2
// gives no public IPv4 to an instance with more than one network interface, so
// spored can never disassociate it and then release it. Until #1287 the address
// waited for the 10-minute sweep and its 15-minute age grace, holding one of the
// region's five default Elastic IP quota slots while a fan-out relaunched.
//
// So an EventBridge rule in each launch region of each member account matches
// "EC2 Instance State-change Notification" with state "terminated" and forwards
// it to this reaper's event bus (deployment/cloudformation/
// ttl-reaper-instance-events.yaml; the self-hosted `spawn reaper deploy` puts the
// same rule on its own default bus). This handler releases that one instance's
// launch Elastic IPs straight away, through the same pkg/elasticip release and
// tag checks as everything else, with no age grace: EC2 has said the instance
// is terminated, and this path asks EC2 again before acting.
//
// The sweep stays as the backstop, for a region without the rule, a dropped
// event, or an address EC2 still reports in use.

const (
	ec2EventSource           = "aws.ec2"
	ec2StateChangeDetailType = "EC2 Instance State-change Notification"
)

// instanceEvent is the slice of an EventBridge event this handler reads.
type instanceEvent struct {
	Source     string `json:"source"`
	DetailType string `json:"detail-type"`
	Account    string `json:"account"`
	Region     string `json:"region"`
	Detail     struct {
		InstanceID string `json:"instance-id"`
		State      string `json:"state"`
	} `json:"detail"`
}

// parseInstanceEvent reports whether raw is an event from EC2. Any other
// payload (the schedule's "Scheduled Event", a manual invoke with {} or none)
// runs the full scan, as every invocation did before #1287. An EC2 event never
// does: it is handled, or ignored, by handleInstanceEvent alone, so a burst of
// terminations cannot turn into a burst of full scans.
func parseInstanceEvent(raw json.RawMessage) (instanceEvent, bool) {
	var ev instanceEvent
	if len(raw) == 0 || json.Unmarshal(raw, &ev) != nil {
		return instanceEvent{}, false
	}
	return ev, ev.Source == ec2EventSource
}

var instanceIDPattern = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)

// eventInUseAttempts and defaultEventInUseWait bound the retry when EC2 still
// reports an address in use: the terminated event can arrive a moment before
// the primary interface is deleted. Past that, the sweep releases it.
const (
	eventInUseAttempts    = 4
	defaultEventInUseWait = 15 * time.Second
)

// accountByID returns the configured account an event came from. An account
// the reaper is not configured for is never acted on: the event bus admits only
// the configured member accounts, and this is the same rule on this side.
func (r *reaper) accountByID(id string) (account, bool) {
	if id == "" {
		return account{}, false
	}
	for _, a := range r.accounts {
		if a.accountID == id {
			return a, true
		}
	}
	return account{}, false
}

// handleInstanceEvent releases the launch Elastic IPs of the one instance a
// terminated state-change event names. Governed by REAPER_ELASTIC_IPS and
// REAPER_DRY_RUN exactly as the sweep is: `reap` releases, `report` (or a dry
// run) logs WOULD release for each address and releases nothing, `off` does
// nothing.
func (r *reaper) handleInstanceEvent(ctx context.Context, ev instanceEvent) Summary {
	sum := Summary{Event: "instance-terminated"}
	id := ev.Detail.InstanceID
	switch {
	case ev.DetailType != ec2StateChangeDetailType || ev.Detail.State != string(ec2types.InstanceStateNameTerminated):
		log.Printf("instance event: ignored %q (state %q) from %s/%s: only terminated state changes are handled",
			ev.DetailType, ev.Detail.State, ev.Account, ev.Region)
		return sum
	case !instanceIDPattern.MatchString(id):
		log.Printf("instance event: ignored a terminated event with instance id %q from %s/%s", id, ev.Account, ev.Region)
		return sum
	}
	sum.EventInstance = id
	acct, ok := r.accountByID(ev.Account)
	if !ok {
		log.Printf("instance event: ignored %s: account %q is not one this reaper is configured for", id, ev.Account)
		return sum
	}
	if !slices.Contains(r.regions, ev.Region) {
		log.Printf("instance event: ignored %s in %s/%s: region not in REAPER_REGIONS %v", id, acct.label, ev.Region, r.regions)
		return sum
	}
	if r.elasticIPs == elasticIPsOff || acct.netFor == nil {
		return sum
	}
	sum.Accounts = 1
	cli := acct.netFor(ev.Region)

	// Ask EC2 rather than trust the event. Absent is fine (it has aged out);
	// any other state means this is not a termination, whatever the event says.
	state, err := instanceState(ctx, cli, id)
	if err != nil {
		log.Printf("instance event: %s/%s: describe %s: %v (the net-resources sweep retries)", acct.label, ev.Region, id, err)
		sum.Errors++
		return sum
	}
	if state != "" && state != ec2types.InstanceStateNameTerminated {
		log.Printf("instance event: %s/%s: %s is %s, not terminated; nothing released", acct.label, ev.Region, id, state)
		return sum
	}

	if r.elasticIPsReportOnly() {
		addrs, err := elasticip.AddressesForInstance(ctx, cli, id)
		if err != nil {
			log.Printf("instance event: %s/%s: %v (the net-resources sweep retries)", acct.label, ev.Region, err)
			sum.Errors++
			return sum
		}
		for _, a := range addrs {
			sum.ElasticIPsScanned++
			sum.ElasticIPsWouldRelease++
			log.Printf("WOULD release Elastic IP %s (%s) in %s/%s — its instance %s terminated (held back: %s)",
				awssdk.ToString(a.AllocationId), awssdk.ToString(a.PublicIp), acct.label, ev.Region, id, r.elasticIPsHeldBy())
		}
		return sum
	}

	wait := r.eventInUseWait
	if wait == 0 {
		wait = defaultEventInUseWait
	}
	released := map[string]bool{}
	for attempt := 1; ; attempt++ {
		res, err := elasticip.ReleaseForInstance(ctx, cli, id)
		for _, a := range res.Released {
			if !released[a] {
				released[a] = true
				sum.ElasticIPsReleased++
				log.Printf("RELEASED Elastic IP %s of terminated instance %s in %s/%s (instance event)", a, id, acct.label, ev.Region)
			}
		}
		if err != nil {
			log.Printf("instance event: %s/%s: release Elastic IP of %s: %v (the net-resources sweep retries)", acct.label, ev.Region, id, err)
			sum.Errors++
		}
		if len(res.InUse) == 0 {
			break
		}
		if attempt >= eventInUseAttempts {
			for _, a := range res.InUse {
				sum.ElasticIPsSkipped++
				log.Printf("Elastic IP %s of terminated instance %s in %s/%s is still associated after %d attempts; the net-resources sweep releases it",
					a, id, acct.label, ev.Region, attempt)
			}
			break
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return sum
			case <-time.After(wait):
			}
		}
	}
	sum.ElasticIPsScanned = len(released) + sum.ElasticIPsSkipped
	return sum
}

// instanceState is the instance's state, or "" when EC2 no longer lists it.
// A filter rather than InstanceIds, which fails outright for an instance that
// has aged out of DescribeInstances.
func instanceState(ctx context.Context, cli netResourceAPI, id string) (ec2types.InstanceStateName, error) {
	out, err := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{{Name: awssdk.String("instance-id"), Values: []string{id}}},
	})
	if err != nil {
		return "", err
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			if awssdk.ToString(inst.InstanceId) == id && inst.State != nil {
				return inst.State.Name, nil
			}
		}
	}
	return "", nil
}
