package provider

import (
	"context"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/observability"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1308: spored's metrics, tracing and idle CPU settings set at launch reach
// spored, against real EC2 tags: pkg/aws's Launch writes them and the parser
// spored itself uses reads them back. Before, nothing in spawn wrote these
// tags, so the only route was create-tags plus a spored restart.
func TestSporedSettingsSurviveTheTagHandOff(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	ami := testutil.RegisterTestAMI(t, env.EC2Client())
	c := spawnaws.NewClientFromConfig(env.AWSConfig)

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

	sampling := 0.25
	got := launch(spawnaws.LaunchConfig{
		Name:           "obs",
		IdleCPUPercent: 12.5,
		Observability: spawnaws.SporedObservability{
			MetricsEnabled: true, MetricsPort: 9100, MetricsBind: "0.0.0.0", MetricsPath: "/prom",
			TracingEnabled: true, TracingSampling: &sampling,
		},
	})
	want := observability.Config{
		Metrics: observability.MetricsConfig{Enabled: true, Port: 9100, Bind: "0.0.0.0", Path: "/prom"},
		Tracing: observability.TracingConfig{Enabled: true, Exporter: "stdout", SamplingRate: 0.25},
	}
	if got.Observability.Metrics != want.Metrics {
		t.Errorf("spored's metrics config = %+v, want %+v", got.Observability.Metrics, want.Metrics)
	}
	if got.Observability.Tracing != want.Tracing {
		t.Errorf("spored's tracing config = %+v, want %+v (the exporter must be stdout: spored's own default, xray, has no permission)",
			got.Observability.Tracing, want.Tracing)
	}
	if got.IdleCPUPercent != 12.5 {
		t.Errorf("spored's idle CPU threshold = %v, want 12.5", got.IdleCPUPercent)
	}

	// Nothing set: spored keeps its own defaults.
	got = launch(spawnaws.LaunchConfig{Name: "plain"})
	if got.Observability != observability.DefaultConfig() || got.IdleCPUPercent != 5.0 {
		t.Errorf("an unconfigured launch changed spored's defaults: %+v, idle cpu %v", got.Observability, got.IdleCPUPercent)
	}
}
