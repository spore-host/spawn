package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spore-host/spawn/pkg/spotrequest"
)

// Cancelling a sweep terminates its members here (#1349).
//
// A cancel is a flag on the sweep record: the dashboard and `spawn cancel` set
// status CANCELLED and cancel_requested. Only this function may terminate a
// member in the member account: SpawnSweepCrossAccountRole trusts only
// SpawnSweepOrchestratorRole. The dashboard used to assume that role itself,
// was always denied, and the members ran on until their TTL. The owner decided
// (spawn#1349) that the dashboard marks the sweep cancelled and invokes this
// function, and this function terminates.
//
// It runs on every path that observes a cancel: a polling loop that reloads
// the record and finds it cancelled, and an invocation that starts on a
// cancelled record (the dashboard's, or a self-reinvocation that was in flight
// when the cancel landed). It is idempotent: members are found by their tags,
// and only members still pending, running, stopping or stopped are terminated,
// so a second run finds nothing to do and writes nothing.

// memberEC2API is what a cancel asks of EC2 in one region. *ec2.Client
// satisfies it.
type memberEC2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CancelSpotInstanceRequests(ctx context.Context, in *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
}

// errSweepCancelled is saveSweepState refusing to overwrite a record that was
// cancelled after this invocation last read it.
var errSweepCancelled = errors.New("sweep was cancelled; record not overwritten")

// Seams, so the cancel path can be driven without AWS.
var (
	loadState           = loadSweepState
	memberClientsFor    = assumeMemberClients
	recordCancelOutcome = writeCancelOutcome
)

// isCancelled reports whether the record asks for a cancel.
func isCancelled(state *SweepRecord) bool {
	return state.CancelRequested || state.Status == "CANCELLED"
}

// sweepRegions is every region a member of state can be in.
func sweepRegions(state *SweepRecord) []string {
	seen := map[string]bool{}
	add := func(r string) {
		if r != "" {
			seen[r] = true
		}
	}
	add(state.Region)
	for r := range state.RegionStatus {
		add(r)
	}
	for _, inst := range state.Instances {
		add(inst.Region)
	}
	regions := make([]string, 0, len(seen))
	for r := range seen {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	return regions
}

// assumeMemberClients assumes SpawnSweepCrossAccountRole once and returns an
// EC2 client for every region the sweep's members can be in.
func assumeMemberClients(ctx context.Context, state *SweepRecord) (map[string]memberEC2API, error) {
	result, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(fmt.Sprintf(crossAccountRole, state.AWSAccountID)),
		RoleSessionName: aws.String("spawn-sweep-orchestrator"),
		DurationSeconds: aws.Int32(3600),
	})
	if err != nil {
		return nil, fmt.Errorf("assume role failed: %w", err)
	}
	clients := map[string]memberEC2API{}
	for _, region := range sweepRegions(state) {
		c, err := createEC2ClientWithCredentials(ctx, region, result.Credentials)
		if err != nil {
			return nil, fmt.Errorf("ec2 client for %s: %w", region, err)
		}
		clients[region] = c
	}
	return clients, nil
}

// liveMemberStates are the instance states a cancel terminates.
var liveMemberStates = []string{"pending", "running", "stopping", "stopped"}

// isSweepMember is the ownership check: the instance carries this sweep's ID
// and spawn:managed=true, the tags this function writes on every member
// (memberTags). It is checked on every instance EC2 returns, not left to the
// request's filter.
func isSweepMember(inst ec2types.Instance, sweepID string) bool {
	var id, managed string
	for _, t := range inst.Tags {
		switch aws.ToString(t.Key) {
		case "spawn:sweep-id":
			id = aws.ToString(t.Value)
		case "spawn:managed":
			managed = aws.ToString(t.Value)
		}
	}
	return sweepID != "" && id == sweepID && managed == "true"
}

// terminateSweepMembers terminates every live member of sweepID that api can
// see, cancelling each Spot member's request first (#922), and returns how many
// it moved from a live state to shutting-down.
func terminateSweepMembers(ctx context.Context, api memberEC2API, region, sweepID string) (int, error) {
	var members []ec2types.Instance
	in := &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:spawn:sweep-id"), Values: []string{sweepID}},
		{Name: aws.String("tag:spawn:managed"), Values: []string{"true"}},
		{Name: aws.String("instance-state-name"), Values: liveMemberStates},
	}}
	for {
		out, err := api.DescribeInstances(ctx, in)
		if err != nil {
			return 0, fmt.Errorf("describe members in %s: %w", region, err)
		}
		for _, res := range out.Reservations {
			for _, inst := range res.Instances {
				if !isSweepMember(inst, sweepID) || inst.State == nil {
					continue
				}
				switch inst.State.Name {
				case ec2types.InstanceStateNamePending, ec2types.InstanceStateNameRunning,
					ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameStopped:
					members = append(members, inst)
				}
			}
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		in.NextToken = out.NextToken
	}
	if len(members) == 0 {
		return 0, nil
	}

	ids := make([]string, 0, len(members))
	for _, inst := range members {
		id := aws.ToString(inst.InstanceId)
		// Cancel first: terminating reopens a persistent request, which launches
		// a replacement. Best effort, as on spawn's other unattended terminate
		// paths: a one-time request (every member this function launches) closes
		// with its instance anyway.
		if reqID := aws.ToString(inst.SpotInstanceRequestId); reqID != "" {
			if err := spotrequest.Cancel(ctx, api, reqID); err != nil {
				log.Printf("WARNING: cancelling sweep %s: %s's Spot request %s was not cancelled: %v",
					sweepID, id, reqID, err)
			}
		}
		ids = append(ids, id)
	}

	terminated := 0
	const batch = 100
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		out, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids[start:end]})
		if err != nil {
			return terminated, fmt.Errorf("terminate members in %s: %w", region, err)
		}
		for _, ch := range out.TerminatingInstances {
			if ch.PreviousState == nil {
				continue
			}
			switch ch.PreviousState.Name {
			case ec2types.InstanceStateNameShuttingDown, ec2types.InstanceStateNameTerminated:
			default:
				terminated++
			}
		}
	}
	log.Printf("Cancelled sweep %s: terminated %d member(s) in %s", sweepID, terminated, region)
	return terminated, nil
}

// cancelSweep terminates the cancelled sweep's live members in every region and
// records the outcome. fresh is the record as last read from DynamoDB. A run
// that terminates nothing on a record already CANCELLED writes nothing, so a
// re-run is a no-op.
func cancelSweep(ctx context.Context, fresh *SweepRecord, clients map[string]memberEC2API) error {
	regions := make([]string, 0, len(clients))
	for r := range clients {
		regions = append(regions, r)
	}
	sort.Strings(regions)

	total := 0
	var errs []error
	for _, r := range regions {
		n, err := terminateSweepMembers(ctx, clients[r], r, fresh.SweepID)
		total += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	if total > 0 || fresh.Status != "CANCELLED" {
		if err := recordCancelOutcome(ctx, fresh.SweepID, total); err != nil {
			errs = append(errs, fmt.Errorf("record cancel: %w", err))
		}
	}
	return errors.Join(errs...)
}

// writeCancelOutcome records a cancel on the sweep record: status CANCELLED,
// cancel_requested, and the members this run terminated added to
// cancel_terminated. An UpdateItem, not a PutItem of this invocation's copy, so
// it cannot clobber what the canceller wrote; completed_at is kept if set.
func writeCancelOutcome(ctx context.Context, sweepID string, terminated int) error {
	now := clock().Format(time.RFC3339)
	_, err := dynamodbClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
		UpdateExpression: aws.String("SET #status = :cancelled, cancel_requested = :t, updated_at = :now, " +
			"completed_at = if_not_exists(completed_at, :now) ADD cancel_terminated :n"),
		ConditionExpression:      aws.String("attribute_exists(sweep_id)"),
		ExpressionAttributeNames: map[string]string{"#status": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":cancelled": &types.AttributeValueMemberS{Value: "CANCELLED"},
			":t":         &types.AttributeValueMemberBOOL{Value: true},
			":now":       &types.AttributeValueMemberS{Value: now},
			":n":         &types.AttributeValueMemberN{Value: fmt.Sprint(terminated)},
		},
	})
	return err
}

// asMemberClients adapts the polling loops' clients.
func asMemberClients(clients map[string]*ec2.Client) map[string]memberEC2API {
	out := make(map[string]memberEC2API, len(clients))
	for r, c := range clients {
		out[r] = c
	}
	return out
}
