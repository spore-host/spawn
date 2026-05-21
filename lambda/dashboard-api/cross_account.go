package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// getCrossAccountRoleARN returns the IAM role ARN used for cross-account EC2 reads.
// Reads from SPAWN_DASHBOARD_CROSS_ACCOUNT_ROLE env var, falls back to hosted default.
func getCrossAccountRoleARN() string {
	if v := os.Getenv("SPAWN_DASHBOARD_CROSS_ACCOUNT_ROLE"); v != "" {
		return v
	}
	return "arn:aws:iam::435415984226:role/SpawnDashboardCrossAccountReadRole"
}

// getEC2ClientForRegion creates an EC2 client for the specified region using the cross-account role
func getEC2ClientForRegion(ctx context.Context, cfg aws.Config, region string) (*ec2.Client, error) {
	// Assume cross-account role
	stsClient := sts.NewFromConfig(cfg)

	// Create credentials provider that assumes the role
	credsProvider := stscreds.NewAssumeRoleProvider(stsClient, getCrossAccountRoleARN(), func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "spawn-dashboard-api"
	})

	// Create config with assumed role credentials for the specific region
	crossAccountCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credsProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load config with cross-account credentials: %w", err)
	}

	// Create EC2 client with cross-account credentials
	return ec2.NewFromConfig(crossAccountCfg), nil
}
