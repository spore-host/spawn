package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/spore-host/spawn/pkg/trustboundary"
)

// The account a sweep acts in is the record's aws_account_id, and the record
// is written by whoever holds PutItem on spawn-sweep-orchestration (#1584).
// Nothing in that write binds the writer to the account it names, so this
// function is a confused deputy for every account whose
// SpawnSweepCrossAccountRole trusts it. Binding writer to target is
// spore-host/spawn#1631; until then, every AssumeRole the orchestrator makes
// goes through assumeSweepRole, which refuses, before STS is called, an
// account no sweep may ever act in, whoever wrote the record:
//
//   - anything that is not a 12-digit account ID (a record naming "*", or an
//     ARN fragment, must not reach a role-ARN template);
//   - the accounts pkg/trustboundary forbids (942542972736): hardcoded there,
//     so no configuration can switch it off;
//   - the account this function runs in, spore.host's control plane, which
//     launches no sweep members. The CLI refuses a sweep role there
//     (pkg/aws CheckSweepRoleTrust); a record naming it is forged or wrong.
//
// The management-account rule is the CLI's, checked with Organizations when
// the role is created: a role in a management account is refused at
// creation, so an AssumeRole into one fails at STS.

var accountIDRe = regexp.MustCompile(`^[0-9]{12}$`)

// checkSweepTarget refuses a target account no sweep may act in. own is the
// orchestrator's own account ("" when unknown, which skips only that check).
func checkSweepTarget(account, own string) error {
	if !accountIDRe.MatchString(account) {
		return fmt.Errorf("refusing to act in account %q: the sweep record's aws_account_id is not a 12-digit account ID", account)
	}
	if why, bad := trustboundary.Forbidden(account); bad {
		return fmt.Errorf("refusing to act in account %s: it is %s", account, why)
	}
	if own != "" && account == own {
		return fmt.Errorf("refusing to act in account %s: it is the account the sweep orchestrator runs in, "+
			"which launches no sweep members; the sweep record names the wrong account", account)
	}
	return nil
}

// ownAccount is the account this invocation runs in, from the invoked
// function's ARN; "" outside Lambda.
func ownAccount(ctx context.Context) string {
	lc, ok := lambdacontext.FromContext(ctx)
	if !ok {
		return ""
	}
	parts := strings.Split(lc.InvokedFunctionArn, ":")
	if len(parts) > 4 && accountIDRe.MatchString(parts[4]) {
		return parts[4]
	}
	return ""
}

// assumeRoleAPI is the STS call assumeSweepRole makes; *sts.Client satisfies it.
type assumeRoleAPI interface {
	AssumeRole(ctx context.Context, in *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// assumeSweepRoleWith assumes SpawnSweepCrossAccountRole in account, after
// checkSweepTarget. The only AssumeRole in this package
// (TestEveryAssumeRoleGoesThroughTheTargetCheck).
func assumeSweepRoleWith(ctx context.Context, api assumeRoleAPI, account string) (*ststypes.Credentials, error) {
	if err := checkSweepTarget(account, ownAccount(ctx)); err != nil {
		return nil, err
	}
	roleARN := fmt.Sprintf(crossAccountRole, account)
	out, err := api.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("spawn-sweep-orchestrator"),
		DurationSeconds: aws.Int32(3600),
	})
	if err != nil {
		return nil, fmt.Errorf("assume %s: %w", roleARN, err)
	}
	return out.Credentials, nil
}

// assumeSweepRole is assumeSweepRoleWith on this function's STS client.
func assumeSweepRole(ctx context.Context, account string) (*ststypes.Credentials, error) {
	return assumeSweepRoleWith(ctx, stsClient, account)
}
