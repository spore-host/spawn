package aws

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/launchconfig"
)

// The launch config object (#1359): the spored settings a launch fixes, which
// used to be instance tags. See pkg/launchconfig for what moved and why.

// launchConfigSettings are the launch-fixed spored settings in config, as the
// strings their spawn: tags used to carry, keyed by pkg/launchconfig's setting
// names. The values are rendered exactly as buildTags rendered the tags, so
// spored parses them as it parsed the tags (TestLaunchConfigObjectHoldsWhat
// TheTagsHeld).
//
// An unparseable webhook timeout is an error, as it was in buildTags: spored
// would ignore it.
func launchConfigSettings(config LaunchConfig) (map[string]string, error) {
	s := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			s[k] = v
		}
	}

	// Notifications and activity signals.
	set(launchconfig.SlackWorkspaceID, config.SlackWorkspaceID)
	set(launchconfig.NotifyURL, config.NotifyURL)
	set(launchconfig.NotifyCommand, config.NotifyCommand)
	set(launchconfig.NotifyPlatform, config.NotifyPlatform)
	set(launchconfig.ActivePorts, config.ActivePortsRaw)
	set(launchconfig.ActiveProcesses, config.ActiveProcessesRaw)

	// Web-UI app mode (#590): spored probes ReadyPort and fronts it with a
	// TLS proxy.
	set(launchconfig.AppMode, config.AppMode)
	if config.ReadyPort > 0 {
		set(launchconfig.ReadyPort, strconv.Itoa(config.ReadyPort))
	}
	set(launchconfig.ReadyPath, config.ReadyHealthPath)
	set(launchconfig.WebAuth, config.WebAuth)

	// Webhooks (#228, #497): only when a URL is set. The correlation blob and
	// timeout are companions, meaningful only with one, and shared by both.
	set(launchconfig.SpotWebhookURL, config.SpotInterruptionWebhookURL)
	set(launchconfig.CompletionWebhookURL, config.CompletionWebhookURL)
	if config.SpotInterruptionWebhookURL != "" || config.CompletionWebhookURL != "" {
		set(launchconfig.WebhookCorrelation, config.WebhookCorrelation)
		if config.WebhookTimeout != "" {
			v, err := lifecycleDurationTag("webhook-timeout", config.WebhookTimeout)
			if err != nil {
				return nil, err
			}
			set(launchconfig.WebhookTimeout, v)
		}
	}

	// spored settings it reads at boot and nothing else could set (#1308).
	// ValidateSporedSettings has refused anything spored would ignore.
	if config.IdleCPUPercent > 0 {
		set(launchconfig.IdleCPU, strconv.FormatFloat(config.IdleCPUPercent, 'f', -1, 64))
	}
	if obs := config.Observability; obs.MetricsEnabled {
		// spawn:metrics-enabled itself stays a tag: Prometheus' EC2 service
		// discovery filters on it (deployment/prometheus/prometheus.yaml).
		if obs.MetricsPort != 0 {
			set(launchconfig.MetricsPort, strconv.Itoa(obs.MetricsPort))
		}
		set(launchconfig.MetricsBind, obs.MetricsBind)
		set(launchconfig.MetricsPath, obs.MetricsPath)
	}
	if obs := config.Observability; obs.TracingEnabled {
		// The exporter is always written: spored's own default is xray, which
		// the instance role cannot reach (SporedObservability.TracingExporter).
		set(launchconfig.TracingEnabled, "true")
		set(launchconfig.TracingExporter, TracingExporterStdout)
		if obs.TracingSampling != nil {
			set(launchconfig.TracingSampling, strconv.FormatFloat(*obs.TracingSampling, 'f', -1, 64))
		}
	}
	return s, nil
}

// LaunchConfigObject is the launch config object a launch of lc writes, as
// its bytes and their sha256, the value of its spawn:config tag. A launch that
// sets none of the launch-fixed settings has no object: body nil, sha "".
//
// [Client.Launch] uploads it to the staging bucket before RunInstances. A
// caller that launches some other way (a detached sweep member, launched by
// the sweep-orchestrator Lambda) seeds it through the user data instead
// (launcher.BootstrapConfig.LaunchConfig).
func LaunchConfigObject(lc LaunchConfig) (body []byte, sha string, err error) {
	if err := lc.ValidateSporedSettings(); err != nil {
		return nil, "", err
	}
	settings, err := launchConfigSettings(lc)
	if err != nil {
		return nil, "", err
	}
	return launchconfig.Encode(settings)
}

// storeLaunchConfigObject uploads a launch's config object to the account's
// staging bucket for region, making sure the bucket exists and is configured
// first. A launch whose object cannot be stored is refused: its instance would
// boot without the settings it was launched with, and nothing would say so
// until a webhook or a notification never arrived.
//
// A caller allowed to write the object but not to set up the bucket (a
// --nested-spawn head node, whose role grants s3:PutObject on config objects
// and nothing that creates or configures a bucket) writes it straight into the
// bucket a workstation launch made, and is warned that its configuration was
// not checked.
func (c *Client) storeLaunchConfigObject(ctx context.Context, region, account string, body []byte, sha string) error {
	bucket := launchconfig.Bucket(account, region)
	ensureErr := c.EnsureStagingBucket(ctx, bucket, region, account)
	if ensureErr != nil && !IsAccessDenied(ensureErr) {
		return fmt.Errorf("refusing to launch: the spored settings this launch fixes (%s) live in a launch "+
			"config object in %s, which could not be made ready: %w", launchConfigSettingNames(body), bucket, ensureErr)
	}
	if err := c.PutS3Object(ctx, region, bucket, launchconfig.Key(sha), account, body); err != nil {
		if ensureErr != nil {
			return fmt.Errorf("refusing to launch: could not store its launch config object (%s) in %s: "+
				"setting the bucket up was refused (%v), and so was the write: %w",
				launchConfigSettingNames(body), bucket, ensureErr, err)
		}
		return fmt.Errorf("refusing to launch: could not store its launch config object (%s): %w",
			launchConfigSettingNames(body), err)
	}
	if ensureErr != nil {
		fmt.Fprintf(os.Stderr, "⚠️  stored the launch config object in %s, but could not check that bucket's "+
			"configuration (%v); a launch from a workstation will.\n", bucket, ensureErr)
	}
	return nil
}

// launchConfigSettingNames names the settings in an object, for a message.
func launchConfigSettingNames(body []byte) string {
	doc, _, err := launchconfig.Decode(body, launchconfig.SHA256Hex(body))
	if err != nil {
		return "unreadable"
	}
	var names []string
	for _, k := range launchconfig.SettingKeys() {
		if _, ok := doc.Settings[k]; ok {
			names = append(names, k)
		}
	}
	return strings.Join(names, ", ")
}

// warnUnmanagedProfileForObject says, on stderr, that an --instance-profile
// spawn did not create may not be able to read the launch config object. spawn
// grants every role it creates s3:GetObject on it; it never edits a profile it
// was handed.
func warnUnmanagedProfileForObject(profile string, body []byte) {
	if body == nil || profile == "" || profile == instancerole.SharedProfileName || strings.HasPrefix(profile, instancerole.NamePrefix) {
		return
	}
	fmt.Fprintf(os.Stderr, "⚠️  --instance-profile %s was not created by spawn. spored reads this launch's %s from %s, "+
		"so its role needs s3:GetObject there; without it spored logs the failure, takes no idle action, and runs "+
		"without those settings.\n", profile, launchConfigSettingNames(body), launchconfig.ObjectARNPattern("<account>"))
}

// launchTagValue is the value of key in tags, or "".
func launchTagValue(tags []types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}
