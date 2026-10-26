package cmd

import (
	"strings"
	"testing"
)

// setSporedFlags sets the #1308 launch flag variables for one test and
// restores them after.
func setSporedFlags(t *testing.T, set func()) {
	t.Helper()
	saved := []any{idleCPUThreshold, metricsEnabled, metricsPort, metricsBind, metricsPath, tracingEnabled, tracingExporter, tracingSampling}
	t.Cleanup(func() {
		idleCPUThreshold, metricsEnabled, metricsPort = saved[0].(string), saved[1].(bool), saved[2].(int)
		metricsBind, metricsPath, tracingEnabled = saved[3].(string), saved[4].(string), saved[5].(bool)
		tracingExporter, tracingSampling = saved[6].(string), saved[7].(string)
	})
	idleCPUThreshold, metricsEnabled, metricsPort, metricsBind, metricsPath = "", false, 0, "", ""
	tracingEnabled, tracingExporter, tracingSampling = false, "", ""
	set()
}

// #1308: the launch flags reach the LaunchConfig that buildTags writes from,
// through buildLaunchConfig, the function single launches, job arrays and
// every sweep row start from.
func TestSporedSettingFlagsReachTheLaunchConfig(t *testing.T) {
	setSporedFlags(t, func() {
		idleCPUThreshold, metricsEnabled, metricsPort, metricsBind, metricsPath = "12.5", true, 9100, "0.0.0.0", "/prom"
		tracingEnabled, tracingSampling = true, "0"
	})
	cfg, err := buildLaunchConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.Observability
	if cfg.IdleCPUPercent != 12.5 || !o.MetricsEnabled || o.MetricsPort != 9100 || o.MetricsBind != "0.0.0.0" || o.MetricsPath != "/prom" || !o.TracingEnabled {
		t.Errorf("config = idle %v, %+v; the flags did not all arrive", cfg.IdleCPUPercent, o)
	}
	if o.TracingSampling == nil || *o.TracingSampling != 0 {
		t.Errorf("--tracing-sampling 0 must be kept as 0 (sample nothing), got %v", o.TracingSampling)
	}
}

// A flag spored would ignore, or one that cannot work, fails the launch.
func TestSporedSettingFlagsAreValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func()
		want string
	}{
		{"bind without --metrics", func() { metricsBind = "0.0.0.0" }, "--metrics"},
		{"sampling without --tracing", func() { tracingSampling = "0.5" }, "--tracing"},
		{"sampling not a number", func() { tracingEnabled, tracingSampling = true, "half" }, "--tracing-sampling"},
		{"xray", func() { tracingEnabled, tracingExporter = true, "xray" }, "xray:PutTraceSegments"},
		{"idle cpu 0", func() { idleCPUThreshold = "0" }, "--idle-cpu-threshold"},
		{"idle cpu not a number", func() { idleCPUThreshold = "lots" }, "--idle-cpu-threshold"},
		{"idle cpu over 100", func() { idleCPUThreshold = "150" }, "percentage"},
		{"port 80", func() { metricsEnabled, metricsPort = true, 80 }, "1024-65535"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSporedFlags(t, tc.set)
			if _, err := buildLaunchConfig(nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}
