// Package orgaccount answers one question: which account is the AWS
// Organizations management account of the organization the caller's account
// is in. spawn refuses to create a cross-account trust into or out of a
// management account (#1537), and asks this before it creates
// SpawnSweepCrossAccountRole.
//
// It is its own package so that pkg/aws, which the Lambdas import, does not
// carry the Organizations client.
package orgaccount

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"
)

// API is the part of the Organizations client ManagementAccount uses.
type API interface {
	DescribeOrganization(ctx context.Context, in *organizations.DescribeOrganizationInput, optFns ...func(*organizations.Options)) (*organizations.DescribeOrganizationOutput, error)
}

// New is the Organizations client for cfg's credentials. Organizations is a
// global service whose endpoint is in us-east-1 in the aws partition, so the
// client signs for us-east-1 whatever region cfg names.
func New(cfg aws.Config) API {
	return organizations.NewFromConfig(cfg, func(o *organizations.Options) { o.Region = "us-east-1" })
}

// ManagementAccount returns the management account of the caller's
// organization, "" with no error when the caller's account is in no
// organization, and an error when it cannot tell (DescribeOrganization is
// denied, or failed). Every member account may call DescribeOrganization, so
// an error is a permission or service problem, not an answer.
func ManagementAccount(ctx context.Context, api API) (string, error) {
	out, err := api.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if err != nil {
		var notInUse *types.AWSOrganizationsNotInUseException
		if errors.As(err, &notInUse) {
			return "", nil
		}
		return "", fmt.Errorf("organizations:DescribeOrganization: %w", err)
	}
	if out == nil || out.Organization == nil || aws.ToString(out.Organization.MasterAccountId) == "" {
		return "", fmt.Errorf("organizations:DescribeOrganization returned no management account")
	}
	return aws.ToString(out.Organization.MasterAccountId), nil
}
