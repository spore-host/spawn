package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// ResolveDNSTarget resolves the custom DNS target (#891) for a launch in region:
// flag, environment, ~/.spawn/config.yaml, then the account's /spawn/dns/* SSM
// parameters in that region — so an account can publish its deployment once.
// See [spawnconfig.ResolveDNSTarget]; the result is unvalidated.
func (c *Client) ResolveDNSTarget(ctx context.Context, region, flagDomain, flagEndpoint string) spawnconfig.DNSTarget {
	return spawnconfig.ResolveDNSTarget(ctx, ssm.NewFromConfig(c.regionalConfig(region)), flagDomain, flagEndpoint)
}
