package cmd

import (
	"context"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// loadAWSConfig is how a spawn command builds an AWS SDK config when it needs a
// raw aws.Config rather than a *spawnaws.Client (#939).
//
// It resolves the way `spawn launch` does, through spawnaws.NewClientWithRegion:
// an explicit region argument wins, then the shared spore.host config (the root
// --region flag, SPORE_REGION, AWS_REGION, ~/.config/spore/config.toml), then
// the SDK's own default chain. The shared profile (--profile, SPORE_PROFILE,
// AWS_PROFILE) is applied the same way. With none of those set it is exactly
// the ambient default chain, so a command that never sees a flag behaves as
// before.
//
// A bare SDK config load reads none of the root's flags. `spawn burst` did
// that, and so launched in the ambient region whatever --region said (#939),
// the same defect as #276 (launch) and #774 (autoscale).
// TestCommandsLoadAWSConfigThroughTheSharedLoader keeps package cmd on this
// path. Commands with a deliberately different account model use
// pkg/config.LoadInfraAWSConfig or LoadComputeAWSConfig instead, which also
// read the shared config.
//
// A package variable so a test can observe the requests a command makes without
// reaching AWS.
var loadAWSConfig = func(ctx context.Context, region string) (sdkaws.Config, error) {
	client, err := spawnaws.NewClientWithRegion(ctx, region)
	if err != nil {
		return sdkaws.Config{}, err
	}
	return client.Config(), nil
}
