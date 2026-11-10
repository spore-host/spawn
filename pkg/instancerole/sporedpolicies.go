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
//     the policy gone at AttachRolePolicy and creates it again.
//
// IAM itself refuses to delete a policy that is attached (DeleteConflict), so
// a policy attached between the listing and the delete is kept.

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

// PolicyAPI is the IAM slice SweepSporedPolicies uses. *iam.Client satisfies
// it.
type PolicyAPI interface {
	iam.ListPoliciesAPIClient
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
	KeepPolicyAttached = "attached to a role"
	KeepPolicyYoung    = "created within the 24h grace"
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
			if aws.ToString(pol.Path) != SporedPolicyPath || !IsSporedPolicyName(name) {
				continue
			}
			d := PolicyDecision{
				Name:        name,
				ARN:         aws.ToString(pol.Arn),
				Created:     aws.ToTime(pol.CreateDate),
				Attachments: aws.ToInt32(pol.AttachmentCount),
			}
			switch {
			case d.Attachments > 0:
				d.Reason = KeepPolicyAttached
			case d.Created.IsZero() || now.Sub(d.Created) < Grace:
				d.Reason = KeepPolicyYoung
			default:
				d.Delete = true
			}
			if d.Delete && reap {
				_, err := api.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: pol.Arn})
				switch {
				case err == nil || isNoSuchEntity(err):
					d.Deleted = true
				case isDeleteConflict(err):
					// Attached since it was listed: kept, which is right.
					d.Delete, d.Reason = false, KeepPolicyAttached
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

func isDeleteConflict(err error) bool {
	var dc *iamtypes.DeleteConflictException
	return errors.As(err, &dc) || (err != nil && strings.Contains(err.Error(), "DeleteConflict"))
}
