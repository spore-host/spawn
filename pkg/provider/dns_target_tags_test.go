package provider

import (
	"context"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

// The whole hand-off for a custom DNS target (#891), against real EC2 tags:
// pkg/aws's Launch writes spawn:dns-domain / spawn:dns-endpoint, and the parser
// spored itself uses reads them back. Either half alone could be right while the
// tag names disagree, which is how a target is silently dropped.
func TestCustomDNSTargetSurvivesTheTagHandOff(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	ami := testutil.RegisterTestAMI(t, env.EC2Client())
	c := spawnaws.NewClientFromConfig(env.AWSConfig)

	const ep = "https://abc123.lambda-url.eu-west-1.on.aws/"
	launch := func(cfg spawnaws.LaunchConfig) *Config {
		t.Helper()
		cfg.InstanceType, cfg.Region, cfg.AMI, cfg.TTL, cfg.PricePerHour = "t3.micro", "us-east-1", ami, "1h", 0.01
		res, err := c.Launch(ctx, cfg)
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		got, _, err := loadConfigFromEC2Tags(ctx, env.EC2Client(), res.InstanceID)
		if err != nil {
			t.Fatalf("loadConfigFromEC2Tags: %v", err)
		}
		return got
	}

	got := launch(spawnaws.LaunchConfig{Name: "web", DNSName: "web", DNSDomain: "spore.example.edu", DNSEndpoint: ep})
	if got.DNSName != "web" || got.DNSDomain != "spore.example.edu" || got.DNSEndpoint != ep {
		t.Errorf("spored read (%q, %q, %q), want (web, spore.example.edu, %s)", got.DNSName, got.DNSDomain, got.DNSEndpoint, ep)
	}

	// No DNS name: a target alone is meaningless and is not written.
	got = launch(spawnaws.LaunchConfig{Name: "quiet", DNSDomain: "spore.example.edu", DNSEndpoint: ep})
	if got.DNSDomain != "" || got.DNSEndpoint != "" {
		t.Errorf("without a DNS name spored still read a target (%q, %q)", got.DNSDomain, got.DNSEndpoint)
	}
}
