// Package keypairsweep deletes the EC2 key pairs spawn imports for one
// session, once nothing uses them (#1621). It is the named deleter for those
// key pairs, shared by the TTL reaper's network-resource sweep and
// `spawn cleanup --session-keys`, so both apply exactly one rule.
//
// Two kinds of key pair carry spawn:managed=true, and they are told apart by
// spawn:purpose, written when the key is imported (pkg/aws ImportKeyPair):
//
//   - PurposeWorkstation ("ssh-key"): the key of ~/.spawn/keys on a user's own
//     machine, imported once and reused by every launch from it. Long-lived and
//     the user's. This package never deletes one: Sweep selects by
//     PurposeSession and re-checks the tag on every key EC2 returns, and the
//     reaper's IAM grant allows DeleteKeyPair only on PurposeSession keys.
//   - PurposeSession ("ssh-key-session"): a key a nested-spawn head imports
//     from its own instance to launch children. Its private half lives on that
//     head and dies with it, so the EC2 key pair is useless once nothing that
//     uses it is left. These used to have no deleter at all.
//
// The rule: a session key is deleted when no instance in the region (pending
// through stopped) was launched with it and it is at least Grace old. The age
// is EC2's own CreateTime for the key pair, which every key pair has.
package keypairsweep

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	// PurposeSession marks a key pair imported for one session (a nested
	// head). The only kind this package deletes.
	PurposeSession = "ssh-key-session"
	// PurposeWorkstation marks a user's long-lived workstation key. Never
	// deleted here.
	PurposeWorkstation = "ssh-key"
	// Grace is how old an unreferenced session key must be. A head imports its
	// key moments before the RunInstances that references it; an hour covers
	// that and any retry loop around it, and a head relaunching children
	// later simply re-imports.
	Grace = time.Hour
)

const (
	managedTag = "spawn:managed"
	purposeTag = "spawn:purpose"
)

// API is the slice of EC2 a sweep uses. *ec2.Client satisfies it.
type API interface {
	DescribeKeyPairs(ctx context.Context, in *ec2.DescribeKeyPairsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DeleteKeyPair(ctx context.Context, in *ec2.DeleteKeyPairInput, optFns ...func(*ec2.Options)) (*ec2.DeleteKeyPairOutput, error)
}

// ClientsFor returns an API for each region, from cfg (the CLI's path; the
// reaper builds its own per-account clients).
func ClientsFor(cfg aws.Config) func(region string) API {
	return func(region string) API {
		c := cfg.Copy()
		c.Region = region
		return ec2.NewFromConfig(c)
	}
}

// Outcome is what a sweep did, or would do, with one session key.
type Outcome string

const (
	Deleted     Outcome = "deleted"
	WouldDelete Outcome = "would-delete"
	InUse       Outcome = "in-use"
	TooYoung    Outcome = "too-young"
	Failed      Outcome = "failed"
)

// Result is one session key and its outcome.
type Result struct {
	KeyName   string
	KeyPairID string
	Age       time.Duration
	Outcome   Outcome
	// UsedBy is an instance launched with the key, for InUse.
	UsedBy string
	Err    error
}

// IsSessionKey reports whether tags mark a spawn session key.
func IsSessionKey(tags []types.Tag) bool {
	var managed, purpose string
	for _, t := range tags {
		switch aws.ToString(t.Key) {
		case managedTag:
			managed = aws.ToString(t.Value)
		case purposeTag:
			purpose = aws.ToString(t.Value)
		}
	}
	return managed == "true" && purpose == PurposeSession
}

// liveStates are the instance states that keep a key pair in use.
var liveStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

// Sweep applies the rule in the region api is for. With dryRun it deletes
// nothing and reports WouldDelete. An error means the region could not be
// read, and nothing was deleted; a failed delete is a Result, not an error.
func Sweep(ctx context.Context, api API, now time.Time, dryRun bool) ([]Result, error) {
	keys, err := api.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:" + managedTag), Values: []string{"true"}},
			{Name: aws.String("tag:" + purposeTag), Values: []string{PurposeSession}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe key pairs: %w", err)
	}
	var session []types.KeyPairInfo
	for _, k := range keys.KeyPairs {
		// The filter is not trusted: a workstation key must never get here.
		if IsSessionKey(k.Tags) {
			session = append(session, k)
		}
	}
	if len(session) == 0 {
		return nil, nil
	}

	usedBy := map[string]string{}
	p := ec2.NewDescribeInstancesPaginator(api, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{{Name: aws.String("instance-state-name"), Values: liveStates}},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, r := range page.Reservations {
			for _, inst := range r.Instances {
				if inst.State != nil && inst.State.Name == types.InstanceStateNameTerminated {
					continue
				}
				if k := aws.ToString(inst.KeyName); k != "" {
					usedBy[k] = aws.ToString(inst.InstanceId)
				}
			}
		}
	}

	out := make([]Result, 0, len(session))
	for _, k := range session {
		r := Result{KeyName: aws.ToString(k.KeyName), KeyPairID: aws.ToString(k.KeyPairId)}
		if k.CreateTime != nil {
			r.Age = now.Sub(*k.CreateTime)
		}
		switch {
		case usedBy[r.KeyName] != "":
			r.Outcome, r.UsedBy = InUse, usedBy[r.KeyName]
		case k.CreateTime == nil || r.Age < Grace:
			r.Outcome = TooYoung
		case r.KeyPairID == "":
			r.Outcome, r.Err = Failed, fmt.Errorf("key pair %s has no id", r.KeyName)
		case dryRun:
			r.Outcome = WouldDelete
		default:
			if _, err := api.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{KeyPairId: aws.String(r.KeyPairID)}); err != nil {
				r.Outcome, r.Err = Failed, err
			} else {
				r.Outcome = Deleted
			}
		}
		out = append(out, r)
	}
	return out, nil
}
