package aws

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
)

// SporedObservability is spored's metrics and tracing configuration. Launch
// writes it as the spawn:metrics-* and spawn:tracing-* tags spored reads when it
// starts (pkg/provider/ec2.go), so it is in effect from the first boot with no
// spored restart (#1308). Zero values leave spored's own defaults in place
// (pkg/observability.DefaultConfig).
type SporedObservability struct {
	// MetricsEnabled starts spored's Prometheus metrics server, and its
	// spawn:metrics-enabled=true tag is what deployment/prometheus's EC2
	// service discovery filters on.
	MetricsEnabled bool
	// MetricsPort is the server's port, 1024-65535 (spored ignores any other);
	// 0 keeps spored's 9090.
	MetricsPort int
	// MetricsBind is the address the server listens on, an IP address or
	// "localhost"; "" keeps spored's localhost, which a scraper on another host
	// cannot reach.
	MetricsBind string
	// MetricsPath is the HTTP path, starting with "/"; "" keeps /metrics.
	MetricsPath string

	// TracingEnabled turns on spored's OpenTelemetry tracing.
	TracingEnabled bool
	// TracingExporter is where spans go. Only "stdout" (spored's journal) is
	// accepted, and "" means stdout. "xray" is refused: the instance role spawn
	// creates grants no xray:PutTraceSegments, so every span would be dropped
	// (#1335).
	TracingExporter string
	// TracingSampling is the fraction of traces kept, 0-1; nil keeps spored's 0.1.
	TracingSampling *float64
}

// TracingExporterStdout is the one tracing exporter a launch can select.
const TracingExporterStdout = "stdout"

// Validate rejects a setting spored would ignore or that cannot work, so it
// fails the launch instead of silently doing nothing.
func (o SporedObservability) Validate() error {
	if !o.MetricsEnabled && (o.MetricsPort != 0 || o.MetricsBind != "" || o.MetricsPath != "") {
		return errors.New("a metrics port, bind address or path does nothing without metrics enabled (--metrics)")
	}
	if o.MetricsPort != 0 && (o.MetricsPort < 1024 || o.MetricsPort > 65535) {
		return fmt.Errorf("invalid metrics port %d: must be 1024-65535 (spored ignores any other)", o.MetricsPort)
	}
	if b := o.MetricsBind; b != "" && b != "localhost" && net.ParseIP(b) == nil {
		return fmt.Errorf("invalid metrics bind address %q: must be an IP address (0.0.0.0 for every interface) or localhost", b)
	}
	if p := o.MetricsPath; p != "" && (!strings.HasPrefix(p, "/") || len(p) > 256 || strings.ContainsFunc(p, func(r rune) bool { return r <= ' ' || r == 0x7f })) {
		return fmt.Errorf("invalid metrics path %q: must start with / and contain no spaces or control characters", p)
	}

	if !o.TracingEnabled && (o.TracingExporter != "" || o.TracingSampling != nil) {
		return errors.New("a tracing exporter or sampling rate does nothing without tracing enabled (--tracing)")
	}
	switch o.TracingExporter {
	case "", TracingExporterStdout:
	case "xray":
		return errors.New("the xray tracing exporter is not available: the instance role spawn creates does not grant " +
			"xray:PutTraceSegments, so every span would be dropped (#1335); use the stdout exporter (spans go to spored's journal)")
	default:
		return fmt.Errorf("invalid tracing exporter %q: must be %s", o.TracingExporter, TracingExporterStdout)
	}
	if s := o.TracingSampling; s != nil && (math.IsNaN(*s) || *s < 0 || *s > 1) {
		return fmt.Errorf("invalid tracing sampling rate %v: must be between 0 and 1", *s)
	}
	return nil
}

// validateIdleCPU checks spored's idle CPU threshold (spawn:idle-cpu): the CPU
// percentage below which the instance counts as idle. 0 means unset (spored's
// 5%); a threshold must be above 0, since 0 would mean the CPU is never below
// it and the idle stop never fires.
func validateIdleCPU(pct float64) error {
	if math.IsNaN(pct) || pct < 0 || pct > 100 {
		return fmt.Errorf("invalid idle CPU threshold %v: must be a percentage above 0 and at most 100", pct)
	}
	return nil
}

// ValidateSporedSettings checks the spored settings a launch writes as tags
// (#1308). Launch checks them in buildTags; a caller that does AWS work of its
// own first should call this before it, like ValidateDurations.
func (c LaunchConfig) ValidateSporedSettings() error {
	if err := c.Observability.Validate(); err != nil {
		return err
	}
	return validateIdleCPU(c.IdleCPUPercent)
}
