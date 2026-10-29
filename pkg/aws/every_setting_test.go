package aws

// everyLaunchConfigSetting sets every LaunchConfig field that writes a spored
// setting a launch fixes (#1359), with distinct values, and enough of the rest
// to be a realistic launch that fits AWS's 50-tag limit on the v0.149.0 tag
// builder: testdata/every-setting-tags.v0.149.0.json is that builder's output
// for this config, captured before the launch config object existed.
func everyLaunchConfigSetting() LaunchConfig {
	sampling := 0.25
	return LaunchConfig{
		InstanceType: "c6i.large", Region: "us-east-1", AMI: "ami-x", KeyName: "k",
		Name: "every", TTL: "4h", IdleTimeout: "1h", CostLimit: 10, PricePerHour: 0.085, PriceSource: "live",
		Username: "ec2-user", SpawnVersion: "0.149.0", TargetOS: "linux",
		OnComplete: "terminate", CompletionFile: "/tmp/C", CompletionDelay: "1m",
		SpotInterruptionWebhookURL: "https://spot.example/hook?token=s3cret", WebhookCorrelation: "corr-1",
		WebhookTimeout: "7s", CompletionWebhookURL: "https://done.example/hook?token=s3cret",
		NotifyURL: "https://notify.example/u?token=n", NotifyCommand: "/spore", SlackWorkspaceID: "T0123",
		NotifyPlatform: "teams",
		ActivePortsRaw: "8888, 6006", ActiveProcessesRaw: "jupyter,python3",
		AppMode: "web", ReadyPort: 8888, ReadyHealthPath: "/healthz", WebAuth: "none",
		IdleCPUPercent: 12.5,
		Observability: SporedObservability{
			MetricsEnabled: true, MetricsPort: 9101, MetricsBind: "0.0.0.0", MetricsPath: "/metricz",
			TracingEnabled: true, TracingSampling: &sampling,
		},
	}
}
