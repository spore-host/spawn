package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// Instance reads are made WITH THE CALLER'S OWN CREDENTIALS, in the caller's
// own account, and nothing else (#1585).
//
// They used to assume SpawnDashboardCrossAccountReadRole in one hardcoded
// member account (435415984226), with the caller's credentials (#815). That
// role trusted Cognito_SpawnDashboard_Auth_Role, the identity-pool role every
// Google, GitHub or Globus sign-in receives, so any of them could assume it
// straight from the browser and DescribeInstances every tenant's spores there;
// the spawn:iam-user filter this Lambda applies was the only isolation. The
// identity-pool role lost the AssumeRole and its EC2 grants on 2026-11-11, and
// the read role is retired: its setup script is deleted and the role itself
// must be deleted by hand (CHANGELOG, #1585).
//
// What replaces it holds no standing access at all. A caller whose
// credentials can read EC2 (a portal sign-in, spore-portal-launch in its own
// account; a CLI user's own keys) reads its own account with them, and IAM,
// not this Lambda, bounds what it sees. A caller whose credentials cannot (an
// identity-pool session) gets errNoCallerCredentials, before any EC2 call.
// This Lambda's own role is never used for these reads.

// errNoCallerCredentials means the request carried no credentials that can
// read instances: none at all (the API Gateway IAM path identifies a caller
// but yields nothing to act with), or a Cognito identity-pool session, which
// has no EC2 access by design.
//
// Failing here is deliberate. The alternative is this Lambda's own
// credentials, which would be a standing read of every tenant's instances from
// behind a public API. An error that names the cause beats a read that
// silently returns nothing.
var errNoCallerCredentials = errors.New(
	"instance reads are made with the caller's own AWS credentials, and this request carried none " +
		"that can read EC2 (a dashboard sign-in through the Cognito identity pool has no EC2 access); " +
		"sign in through the portal, or use `spawn list`")

// identityPoolSessionName is the role session name Cognito identity-pool
// credentials (GetCredentialsForIdentity) always carry.
const identityPoolSessionName = "CognitoIdentityCredentials"

// isIdentityPoolSession reports whether arn is a Cognito identity-pool
// session: arn:aws:sts::<acct>:assumed-role/<role>/CognitoIdentityCredentials.
func isIdentityPoolSession(arn string) bool {
	return strings.HasPrefix(arn, "arn:") && strings.Contains(arn, ":assumed-role/") && strings.HasSuffix(arn, "/"+identityPoolSessionName)
}

// getEC2ClientForRegion builds an EC2 client for region on the CALLER's
// credentials.
//
// callerCreds comes from the X-AWS-Credentials header, after
// getUserFromCredentialsHeader has validated it against STS, so it is a
// verified identity, not a claim.
func getEC2ClientForRegion(ctx context.Context, callerCreds aws.CredentialsProvider, region string) (*ec2.Client, error) {
	if callerCreds == nil {
		return nil, errNoCallerCredentials
	}
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(callerCreds),
	)
	if err != nil {
		return nil, fmt.Errorf("load config with the caller's credentials: %w", err)
	}
	return ec2.NewFromConfig(cfg), nil
}
