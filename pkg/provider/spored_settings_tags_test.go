package provider

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launchconfig"
	"github.com/spore-host/spawn/pkg/observability"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1308, #1359: spored's metrics, tracing and idle CPU settings set at launch
// reach spored, against real EC2 tags and a real S3 object: the launch's tags
// (spawn:config among them) on an instance, its launch config object in the
// staging bucket, and the loader spored itself uses reading both back.
//
// Client.Launch is not used: it also sets up the staging bucket, and Substrate
// does not implement PutBucketOwnershipControls. The tags and the object are
// what Launch writes: BuildLaunchTags and LaunchConfigObject are its builders.
func TestSporedSettingsSurviveTheTagHandOff(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	ami := testutil.RegisterTestAMI(t, env.EC2Client())
	const account = "123456789012"
	bucket := launchconfig.Bucket(account, "us-east-1")
	s3c := s3.NewFromConfig(env.AWSConfig, func(o *s3.Options) { o.UsePathStyle = true })
	if _, err := s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	launch := func(cfg spawnaws.LaunchConfig) *Config {
		t.Helper()
		cfg.InstanceType, cfg.Region, cfg.AMI, cfg.TTL, cfg.PricePerHour = "t3.micro", "us-east-1", ami, "1h", 0.01
		tags, err := spawnaws.BuildLaunchTags(cfg, account, "arn:aws:iam::"+account+":user/u", "")
		if err != nil {
			t.Fatalf("BuildLaunchTags: %v", err)
		}
		body, sha, err := spawnaws.LaunchConfigObject(cfg)
		if err != nil {
			t.Fatalf("LaunchConfigObject: %v", err)
		}
		if body != nil {
			if _, err := s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(launchconfig.Key(sha)), Body: bytes.NewReader(body)}); err != nil {
				t.Fatalf("PutObject: %v", err)
			}
		}
		out, err := env.EC2Client().RunInstances(ctx, &ec2.RunInstancesInput{
			InstanceType: ec2types.InstanceTypeT3Micro, ImageId: aws.String(ami),
			MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}},
		})
		if err != nil {
			t.Fatalf("RunInstances: %v", err)
		}
		loader := &launchConfigLoader{s3: s3c, bucket: bucket, cachePath: filepath.Join(t.TempDir(), "launch-config.json")}
		got, _, err := loadConfig(ctx, env.EC2Client(), aws.ToString(out.Instances[0].InstanceId), loader)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if got.LaunchConfigError != "" {
			t.Fatalf("the launch config object did not load: %s", got.LaunchConfigError)
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
	if got.LaunchConfigSource != string(launchconfig.FromS3) {
		t.Errorf("the object came from %q, want s3 on a first boot", got.LaunchConfigSource)
	}

	// Nothing set: no object, and spored keeps its own defaults.
	got = launch(spawnaws.LaunchConfig{Name: "plain"})
	if got.Observability != observability.DefaultConfig() || got.IdleCPUPercent != 5.0 || got.LaunchConfigSHA != "" {
		t.Errorf("an unconfigured launch changed spored's defaults: %+v, idle cpu %v, config %q", got.Observability, got.IdleCPUPercent, got.LaunchConfigSHA)
	}
}
