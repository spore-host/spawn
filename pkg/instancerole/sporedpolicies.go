package instancerole

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spore-host/libs/tags"
)

// The spored managed policies (#1573). pkg/aws attaches spored's grants to the
// spored roles as customer-managed policies at SporedPolicyPath, one per grant
// document per policy schema per account. Delete detaches them with every
// other managed policy when it removes a role, and pkg/aws deletes an older
// schema's policy when it detaches it from the last role. What can still be
// left is a policy no role has attached: an older schema's, once its last role
// was reaped rather than relaunched, or one a launch created and then could
// not attach. SweepSporedPolicies deletes those.
//
// It deletes a policy only when all of these hold:
//
//   - it is at SporedPolicyPath with a name spawn writes there
//     (IsSporedPolicyName), so a policy spawn did not create is never touched;
//   - IAM reports it attached to nothing (AttachmentCount 0);
//   - it was created more than Grace ago, so a launch between creating it and
//     attaching it is not raced. A launch that loses that race anyway finds
//     the policy gone at AttachRolePolicy and creates it again;
//   - its tags could be read and it is not tagged spore-host:permanent=true.
//
// IAM itself refuses to delete a policy that is attached (DeleteConflict), so
// a policy attached between the listing and the delete is kept.
//
// The spawn-instance permissions boundary, at the same path, is never deleted
// here (#1664). It is permanent account setup, one of the two pieces of
// spore.host kept in an account while it is set up, with the user lifecycle
// policy (spore-host/spore-host#752): an administrator creates it with `spawn
// setup-account`, the documented user baseline cannot re-create it, so a sweep
// that removed it once no role used it broke every non-admin's next launch.
// It is listed, as kept, and only `spawn setup-account --remove` (#1656)
// removes it.

// SporedPolicyPath is pkg/aws.SporedPolicyPath, repeated so the reaper need
// not import pkg/aws (a test holds the two equal).
const SporedPolicyPath = "/spore-host/"

// sporedPolicyBases are pkg/aws.SporedManagedPolicyBases (a test holds the two
// equal).
var sporedPolicyBases = []string{"spored-policy", "spored-baseline-policy", "spored-nested-baseline-policy"}

// IsSporedPolicyName reports whether name is one spawn gives a managed policy
// at SporedPolicyPath: one of the bases, "-v", and a schema number from 1.
func IsSporedPolicyName(name string) bool {
	for _, b := range sporedPolicyBases {
		rest, ok := strings.CutPrefix(name, b+"-v")
		if !ok || rest == "" {
			continue
		}
		digits := true
		for _, r := range rest {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if v, err := strconv.Atoi(rest); digits && err == nil && v >= 1 {
			return true
		}
	}
	return false
}

// BoundaryPolicyName is the spawn-instance permissions boundary's name at
// SporedPolicyPath (pkg/aws.spawnInstanceBoundaryName; a test holds the two
// equal). SweepSporedPolicies names it so that it can keep it: it is never
// deleted here (#1664).
const BoundaryPolicyName = "spawn-instance-boundary"

// KeepBoundaryPermanent is the reason the boundary policy is kept, always.
const KeepBoundaryPermanent = "the spawn-instance permissions boundary is permanent account setup; only 'spawn setup-account --remove' removes it"

// PolicyAPI is the IAM slice SweepSporedPolicies uses. *iam.Client satisfies
// it. It has no policy-version calls: nothing it deletes has non-default
// versions (only the boundary does, and the boundary is never deleted).
type PolicyAPI interface {
	iam.ListPoliciesAPIClient
	iam.ListPolicyTagsAPIClient
	DeletePolicy(ctx context.Context, in *iam.DeletePolicyInput, optFns ...func(*iam.Options)) (*iam.DeletePolicyOutput, error)
}

// NewPolicyClient is the IAM client for SweepSporedPolicies, from cfg.
func NewPolicyClient(cfg aws.Config) PolicyAPI { return iam.NewFromConfig(cfg) }

// PolicyDecision is the outcome for one spored managed policy.
type PolicyDecision struct {
	Name, ARN   string
	Created     time.Time
	Attachments int32
	// Delete: it may go. Deleted: it went. Err: the delete failed.
	Delete, Deleted bool
	Reason          string
	Err             error
}

// Reasons a spored managed policy is kept.
const (
	KeepPolicyAttached  = "attached to a role"
	KeepPolicyYoung     = "created within the 24h grace"
	KeepPolicyPermanent = "tagged " + tags.Permanent + "=" + tags.PermanentTrue
	// KeepPolicyTagsUnreadable: its tags could not be read, so it cannot be
	// shown not to be permanent (needs iam:ListPolicyTags).
	KeepPolicyTagsUnreadable = "its tags could not be read (iam:ListPolicyTags), so it cannot be shown not to be permanent"
)

// SweepSporedPolicies lists spawn's spored managed policies and, when reap is
// true, deletes the ones nothing has attached that are older than Grace. With
// reap false nothing is changed.
func SweepSporedPolicies(ctx context.Context, api PolicyAPI, now time.Time, reap bool) ([]PolicyDecision, error) {
	var out []PolicyDecision
	p := iam.NewListPoliciesPaginator(api, &iam.ListPoliciesInput{
		Scope:      iamtypes.PolicyScopeTypeLocal,
		PathPrefix: aws.String(SporedPolicyPath),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("list managed policies at %s: %w", SporedPolicyPath, err)
		}
		for _, pol := range page.Policies {
			name := aws.ToString(pol.PolicyName)
			if aws.ToString(pol.Path) != SporedPolicyPath {
				continue
			}
			d := PolicyDecision{
				Name:        name,
				ARN:         aws.ToString(pol.Arn),
				Created:     aws.ToTime(pol.CreateDate),
				Attachments: aws.ToInt32(pol.AttachmentCount),
			}
			// The boundary first, by name or ARN, before anything that could
			// make it a candidate: it is permanent (#1664).
			if isBoundary(name, d.ARN) {
				d.Attachments += aws.ToInt32(pol.PermissionsBoundaryUsageCount)
				d.Reason = KeepBoundaryPermanent
				out = append(out, d)
				continue
			}
			if !IsSporedPolicyName(name) {
				continue
			}
			switch {
			case d.Attachments > 0:
				d.Reason = KeepPolicyAttached
			case d.Created.IsZero() || now.Sub(d.Created) < Grace:
				d.Reason = KeepPolicyYoung
			default:
				// Only a candidate's tags are read (ListPolicies returns
				// none). Unreadable tags keep it: fail closed.
				t, err := policyTags(ctx, api, pol.Arn)
				switch {
				case err != nil:
					d.Reason, d.Err = KeepPolicyTagsUnreadable, err
				case tags.IsPermanent(t):
					d.Reason = KeepPolicyPermanent
				default:
					d.Delete = true
				}
			}
			if d.Delete && reap {
				_, err := api.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: pol.Arn})
				switch {
				case err == nil || isNoSuchEntity(err):
					d.Deleted = true
				case isDeleteConflict(err):
					// Attached since it was listed: kept, which is right.
					d.Delete = false
					d.Reason = KeepPolicyAttached
				default:
					d.Err = err
				}
			}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// isBoundary reports whether a policy at SporedPolicyPath is the
// spawn-instance boundary, by name or by ARN.
func isBoundary(name, arn string) bool {
	return name == BoundaryPolicyName || strings.HasSuffix(arn, ":policy"+SporedPolicyPath+BoundaryPolicyName)
}

// policyTags reads every tag on a managed policy.
func policyTags(ctx context.Context, api PolicyAPI, arn *string) (map[string]string, error) {
	t := map[string]string{}
	p := iam.NewListPolicyTagsPaginator(api, &iam.ListPolicyTagsInput{PolicyArn: arn})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list tags of %s: %w", aws.ToString(arn), err)
		}
		for _, tg := range page.Tags {
			t[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
	}
	return t, nil
}

func isDeleteConflict(err error) bool {
	var dc *iamtypes.DeleteConflictException
	return errors.As(err, &dc) || (err != nil && strings.Contains(err.Error(), "DeleteConflict"))
}
