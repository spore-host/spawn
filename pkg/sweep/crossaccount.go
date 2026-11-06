package sweep

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// CrossAccountRoleName is the role the sweep orchestrator assumes in the
// account a detached sweep's members launch in. Every member is launched,
// tagged and cancelled through it (lambda/sweep-orchestrator).
const CrossAccountRoleName = "SpawnSweepCrossAccountRole"

// ErrCrossAccountRoleMissing says the account has no CrossAccountRoleName, so
// the orchestrator could launch none of a detached sweep's members.
var ErrCrossAccountRoleMissing = errors.New(CrossAccountRoleName + " does not exist in this account")

// RoleGetter is the slice of the IAM client CheckCrossAccountRole uses.
type RoleGetter interface {
	GetRole(ctx context.Context, in *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
}

// NewRoleGetter is the IAM client CheckCrossAccountRole asks, with cfg's
// credentials: those of the account the members launch in.
func NewRoleGetter(cfg aws.Config) RoleGetter { return iam.NewFromConfig(cfg) }

// CheckCrossAccountRole reports whether CrossAccountRoleName exists, with
// iam:GetRole. The caller cannot test the assume itself: the role trusts only
// the orchestrator's role. It returns nil when the role exists, an error
// wrapping ErrCrossAccountRoleMissing when IAM says it does not, and any other
// error as it came (not authorized to look, throttled), which says nothing
// about whether the role is there.
func CheckCrossAccountRole(ctx context.Context, c RoleGetter) error {
	_, err := c.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(CrossAccountRoleName)})
	if err == nil {
		return nil
	}
	var nse *iamtypes.NoSuchEntityException
	if errors.As(err, &nse) {
		return fmt.Errorf("%w: %v", ErrCrossAccountRoleMissing, err)
	}
	return err
}
