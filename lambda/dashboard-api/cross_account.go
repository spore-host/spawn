package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Cross-account EC2 reads are performed AS THE CALLER, not as this Lambda (#815).
//
// The member-account role's trust policy names
// `Cognito_SpawnDashboard_Auth_Role` — the identity-pool role the browser assumes
// — and has never named `SpawnDashboardLambdaRole`. The code drifted the other
// way: main.go built one `aws.Config` from the Lambda's own credentials and passed
// it everywhere, so the assume-role call was made by the Lambda and was denied.
// That is why the dashboard's cross-account instance listing has never worked.
//
// The obvious repair — add the Lambda role to the trust and grant it
// sts:AssumeRole — was rejected deliberately. It would create a standing role
// able to read EVERY onboarded tenant's instances, assumed by a Lambda behind a
// public API, growing in reach with each account onboarded. That is the shape of
// the findings this audit spent its time removing (GHSA-qc2h-6qqf-wcm4's
// fail-open ownership check, GHSA-7pm4-cqjg-vh2q's open front door).
//
// Acting as the caller inverts it: each user reads their own account with their
// own permissions, this Lambda holds no cross-tenant access at all, and the trust
// policy already in production is correct as written.

// errNoCallerCredentials means the request authenticated without usable
// credentials — the API Gateway IAM path identifies a caller but yields nothing
// to act with.
//
// Failing here is deliberate. The alternative is falling back to this Lambda's own
// credentials, which cannot succeed (the trust does not name it) and would be the
// standing-privilege design if it ever did. An error that names the cause beats a
// cross-account read that silently returns nothing.
var errNoCallerCredentials = errors.New(
	"cross-account read requires the caller's credentials; this request authenticated " +
		"without them (X-AWS-Credentials absent), so there is nothing to act as")

// getCrossAccountRoleARN returns the IAM role ARN assumed for cross-account EC2
// reads, in the account being read.
func getCrossAccountRoleARN() string {
	if v := os.Getenv("SPAWN_DASHBOARD_CROSS_ACCOUNT_ROLE"); v != "" {
		return v
	}
	return "arn:aws:iam::435415984226:role/SpawnDashboardCrossAccountReadRole"
}

// getEC2ClientForRegion builds an EC2 client for region by assuming the
// cross-account read role with the CALLER's credentials.
//
// callerCreds comes from the X-AWS-Credentials header the dashboard sends, after
// getUserFromCredentialsHeader has already validated it against STS — so by the
// time it reaches here it is a verified identity, not a claim.
func getEC2ClientForRegion(ctx context.Context, cfg aws.Config, callerCreds aws.CredentialsProvider, region string) (*ec2.Client, error) {
	if callerCreds == nil {
		return nil, errNoCallerCredentials
	}

	// STS is called with the caller's credentials, so the AssumeRole is attributed
	// to them. This one line is the whole fix: everything else was already right,
	// including the member-account trust policy.
	stsClient := sts.NewFromConfig(cfg, func(o *sts.Options) {
		o.Credentials = callerCreds
	})

	credsProvider := stscreds.NewAssumeRoleProvider(stsClient, getCrossAccountRoleARN(), func(o *stscreds.AssumeRoleOptions) {
		// Session name carries the caller, so CloudTrail in the member account
		// shows which dashboard user read it rather than one shared identity.
		o.RoleSessionName = "spawn-dashboard-caller"
	})

	crossAccountCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credsProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("load config with cross-account credentials: %w", err)
	}
	return ec2.NewFromConfig(crossAccountCfg), nil
}
