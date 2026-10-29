package provider

// legacyConfigFromTagsV0149 is spored's configFromTags as it was in v0.149.0,
// before the launch config object (#1359), kept verbatim as the oracle for
// TestLaunchConfigObjectIsEquivalentToTheTagPath: given the tags v0.149.0's
// launcher wrote, it is the agent config spored ran with. Do not edit it to
// match the new code; it is the "before" the new code is held to.

import (
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/costmodel"
	"github.com/spore-host/spawn/pkg/observability"
	"github.com/spore-host/spawn/pkg/tagprefix"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

func legacyConfigFromTagsV0149(tags []types.TagDescription) (*Config, string) {
	config := &Config{
		IdleCPUPercent: 5.0, // Default
		Observability:  observability.DefaultConfig(),
	}

	var instanceName string
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			continue
		}

		switch *tag.Key {
		case "Name":
			instanceName = *tag.Value
		case tagprefix.Tag("ttl"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.TTL = duration
			}
		case tagprefix.Tag("ttl-deadline"):
			if t, err := time.Parse(time.RFC3339, *tag.Value); err == nil {
				config.TTLDeadline = t
			}
		case tagprefix.Tag("task-id"):
			config.TaskID = *tag.Value
		case tagprefix.Tag("launch-time"):
			if t, err := time.Parse(time.RFC3339, *tag.Value); err == nil {
				config.LaunchTime = t
			}
		case tagprefix.Tag("compute-seconds"):
			if s, err := strconv.ParseInt(*tag.Value, 10, 64); err == nil {
				config.ComputeSeconds = s
			}
		case tagprefix.Tag(costmodel.TagEBSHourlyCost):
			if f, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.EBSHourlyCost = f
			}
		case tagprefix.Tag(costmodel.TagPublicIPv4):
			if n, err := strconv.Atoi(*tag.Value); err == nil && n > 0 {
				config.PublicIPv4Count = n
			}
		case tagprefix.Tag(costmodel.TagElasticIPv4):
			if n, err := strconv.Atoi(*tag.Value); err == nil && n > 0 {
				config.ElasticIPv4Count = n
			}
		case tagprefix.Tag("idle-timeout"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.IdleTimeout = duration
			}
		case tagprefix.Tag("hibernate-on-idle"):
			config.HibernateOnIdle = *tag.Value == "true"
		case tagprefix.Tag("terminate-on-idle"):
			config.TerminateOnIdle = *tag.Value == "true"
		case tagprefix.Tag("cost-limit"):
			if limit, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.CostLimit = limit
			}
		case tagprefix.Tag("price-per-hour"):
			if price, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.PricePerHour = price
			}
		case tagprefix.Tag("spot-price-per-hour"):
			if price, err := strconv.ParseFloat(*tag.Value, 64); err == nil && price > 0 {
				config.SpotPricePerHour = price
			}
		case tagprefix.Tag("idle-cpu"):
			if cpu, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.IdleCPUPercent = cpu
			}
		case tagprefix.Tag("dns-name"):
			config.DNSName = *tag.Value
		case tagprefix.Tag("dns-domain"):
			config.DNSDomain = *tag.Value
		case tagprefix.Tag("dns-endpoint"):
			config.DNSEndpoint = *tag.Value
		case tagprefix.Tag("account-base36"):
			config.AccountBase36 = *tag.Value
		case tagprefix.Tag("account-name"):
			config.AccountName = *tag.Value
		case tagprefix.Tag("notify-url"):
			config.NotifyURL = *tag.Value
		case tagprefix.Tag("notify-command"):
			config.NotifyCommand = *tag.Value
		case tagprefix.Tag("notify-platform"):
			config.NotifyPlatform = *tag.Value
		case tagprefix.Tag("active-ports"):
			for _, p := range strings.Split(*tag.Value, ",") {
				p = strings.TrimSpace(p)
				if port, err := strconv.Atoi(p); err == nil && port > 0 {
					config.ActivePorts = append(config.ActivePorts, port)
				}
			}
		case tagprefix.Tag("active-processes"):
			for _, p := range strings.Split(*tag.Value, ",") {
				if p = strings.TrimSpace(p); p != "" {
					config.ActiveProcesses = append(config.ActiveProcesses, p)
				}
			}
		case tagprefix.Tag("dcv-session-id"):
			config.DCVSessionID = *tag.Value
		case tagprefix.Tag("app-mode"):
			config.AppMode = *tag.Value
		case tagprefix.Tag("ready-port"):
			if port, err := strconv.Atoi(strings.TrimSpace(*tag.Value)); err == nil {
				config.ReadyPort = port
			}
		case tagprefix.Tag("ready-path"):
			config.ReadyHealthPath = *tag.Value
		case tagprefix.Tag("web-auth"):
			config.WebAuth = *tag.Value
		case tagprefix.Tag("slack-workspace-id"):
			config.SlackWorkspaceID = *tag.Value
		case tagprefix.Tag("pre-stop"):
			config.PreStop = *tag.Value
		case tagprefix.Tag("pre-stop-timeout"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.PreStopTimeout = duration
			}
		case tagprefix.Tag("spot-webhook-url"):
			config.SpotWebhookURL = *tag.Value
		case tagprefix.Tag("completion-webhook-url"):
			config.CompletionWebhookURL = *tag.Value
		case tagprefix.Tag("webhook-correlation"):
			config.WebhookCorrelation = *tag.Value
		case tagprefix.Tag("webhook-timeout"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.WebhookTimeout = duration
			}
		case tagprefix.Tag("local-username"):
			config.LocalUsername = *tag.Value
		case tagprefix.Tag("fsx-pending"):
			config.FSxPending = *tag.Value
		case tagprefix.Tag("fsx-mount-point"):
			config.FSxMountPoint = *tag.Value
		case tagprefix.Tag("fsx-s3-import-path"):
			config.FSxImportPath = *tag.Value
		case tagprefix.Tag("fsx-s3-export-path"):
			config.FSxExportPath = *tag.Value
		case tagprefix.Tag("on-complete"):
			config.OnComplete = *tag.Value
		case tagprefix.Tag("completion-file"):
			config.CompletionFile = *tag.Value
		case tagprefix.Tag("completion-delay"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.CompletionDelay = duration
			}
		case tagprefix.Tag("job-array-id"):
			config.JobArrayID = *tag.Value
		case tagprefix.Tag("job-array-name"):
			config.JobArrayName = *tag.Value
		case tagprefix.Tag("job-array-size"):
			if size, err := strconv.Atoi(*tag.Value); err == nil {
				config.JobArraySize = size
			}
		case tagprefix.Tag("job-array-index"):
			if index, err := strconv.Atoi(*tag.Value); err == nil {
				config.JobArrayIndex = index
			}
		case tagprefix.Tag("burst"):
			config.Burst = *tag.Value == "true"

		// Observability - Metrics
		case tagprefix.Tag("metrics-enabled"):
			config.Observability.Metrics.Enabled = *tag.Value == "true"
		case tagprefix.Tag("metrics-port"):
			if port, err := strconv.Atoi(*tag.Value); err == nil && port >= 1024 && port <= 65535 {
				config.Observability.Metrics.Port = port
			}
		case tagprefix.Tag("metrics-bind"):
			config.Observability.Metrics.Bind = *tag.Value
		case tagprefix.Tag("metrics-path"):
			config.Observability.Metrics.Path = *tag.Value

		// Observability - Tracing
		case tagprefix.Tag("tracing-enabled"):
			config.Observability.Tracing.Enabled = *tag.Value == "true"
		case tagprefix.Tag("tracing-exporter"):
			config.Observability.Tracing.Exporter = *tag.Value
		case tagprefix.Tag("tracing-sampling"):
			if rate, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.Observability.Tracing.SamplingRate = rate
			}
		case tagprefix.Tag("tracing-endpoint"):
			config.Observability.Tracing.Endpoint = *tag.Value
		}
	}

	// Set default completion file if on-complete is set but file isn't specified
	if config.OnComplete != "" && config.CompletionFile == "" {
		config.CompletionFile = DefaultCompletionFile
	}

	// Set default completion delay if on-complete is set but delay isn't specified
	if config.OnComplete != "" && config.CompletionDelay == 0 {
		config.CompletionDelay = 30 * time.Second
	}

	return config, instanceName
}
