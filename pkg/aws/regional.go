package aws

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// regionalConfig returns a copy of the client's config pinned to region. An empty
// region returns the config unchanged (default-chain region preserved) — callers
// that may pass "" rely on this to mean "use the ambient/default region".
//
// This replaces the old getRegionalConfig(ctx, region) (aws.Config, error) helper,
// whose returned error was always nil.
func (c *Client) regionalConfig(region string) aws.Config {
	cfg := c.cfg.Copy()
	if region != "" {
		cfg.Region = region
	}
	return cfg
}

// regionalEC2 returns an EC2 client pinned to region (or the default region when
// region is empty). Collapses the repeated
// `cfg := c.cfg.Copy(); cfg.Region = region; ec2.NewFromConfig(cfg)` idiom.
func (c *Client) regionalEC2(region string) *ec2.Client {
	return ec2.NewFromConfig(c.regionalConfig(region))
}
