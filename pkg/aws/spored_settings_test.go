package aws

import (
	"math"
	"strings"
	"testing"
)

// Every spored setting a launch can write is checked before it is written: a
// value spored would ignore, or one that cannot work, fails the launch rather
// than producing an instance that silently does something else (#1308).
func TestSporedSettingsAreValidated(t *testing.T) {
	half, two, nan := 0.5, 2.0, math.NaN()
	for _, tc := range []struct {
		name string
		cfg  LaunchConfig
		want string // "" = valid; else a substring of the error
	}{
		{"nothing set", LaunchConfig{}, ""},
		{"metrics, every field", LaunchConfig{Observability: SporedObservability{MetricsEnabled: true, MetricsPort: 9100, MetricsBind: "0.0.0.0", MetricsPath: "/metrics"}}, ""},
		{"metrics bind localhost", LaunchConfig{Observability: SporedObservability{MetricsEnabled: true, MetricsBind: "localhost"}}, ""},
		{"metrics bind IPv6", LaunchConfig{Observability: SporedObservability{MetricsEnabled: true, MetricsBind: "::"}}, ""},
		{"tracing, stdout, sampled", LaunchConfig{Observability: SporedObservability{TracingEnabled: true, TracingExporter: "stdout", TracingSampling: &half}}, ""},
		{"idle cpu 100", LaunchConfig{IdleCPUPercent: 100}, ""},

		{"metrics port without metrics", LaunchConfig{Observability: SporedObservability{MetricsPort: 9100}}, "without metrics enabled"},
		{"metrics bind without metrics", LaunchConfig{Observability: SporedObservability{MetricsBind: "0.0.0.0"}}, "without metrics enabled"},
		{"privileged port", LaunchConfig{Observability: SporedObservability{MetricsEnabled: true, MetricsPort: 80}}, "1024-65535"},
		{"port too high", LaunchConfig{Observability: SporedObservability{MetricsEnabled: true, MetricsPort: 70000}}, "1024-65535"},
		{"bind a hostname", LaunchConfig{Observability: SporedObservability{MetricsEnabled: true, MetricsBind: "example.com"}}, "IP address"},
		{"path without slash", LaunchConfig{Observability: SporedObservability{MetricsEnabled: true, MetricsPath: "metrics"}}, "start with /"},
		{"path with a space", LaunchConfig{Observability: SporedObservability{MetricsEnabled: true, MetricsPath: "/a b"}}, "start with /"},
		{"sampling without tracing", LaunchConfig{Observability: SporedObservability{TracingSampling: &half}}, "without tracing enabled"},
		{"exporter without tracing", LaunchConfig{Observability: SporedObservability{TracingExporter: "stdout"}}, "without tracing enabled"},
		{"xray has no permission", LaunchConfig{Observability: SporedObservability{TracingEnabled: true, TracingExporter: "xray"}}, "xray:PutTraceSegments"},
		{"unknown exporter", LaunchConfig{Observability: SporedObservability{TracingEnabled: true, TracingExporter: "otlp"}}, "invalid tracing exporter"},
		{"sampling over 1", LaunchConfig{Observability: SporedObservability{TracingEnabled: true, TracingSampling: &two}}, "between 0 and 1"},
		{"sampling NaN", LaunchConfig{Observability: SporedObservability{TracingEnabled: true, TracingSampling: &nan}}, "between 0 and 1"},
		{"idle cpu negative", LaunchConfig{IdleCPUPercent: -1}, "percentage"},
		{"idle cpu over 100", LaunchConfig{IdleCPUPercent: 101}, "percentage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateSporedSettings()
			if tc.want == "" {
				if err != nil {
					t.Errorf("rejected a valid setting: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.want)
			}
			// buildTags is the launch's own check, for SDK callers that skip it.
			if _, berr := buildTags(tc.cfg, "123456789012", "arn:aws:iam::1:user/u", ""); berr == nil {
				t.Errorf("buildTags wrote tags for an invalid setting")
			}
		})
	}
}
