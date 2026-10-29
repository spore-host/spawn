package provider

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/launchconfig"
	"github.com/spore-host/spawn/pkg/tagprefix"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

// spored's half of the launch config object (#1359, pkg/launchconfig).
//
// The launch-fixed settings come from the object the instance's spawn:config
// tag names, and from nowhere else: configFromTags has no case for any of
// them, so a tag with one of their names is ignored (and reported once, see
// warnTagOnlySettings). Everything else still comes from the tags.
//
// When there is no spawn:config tag the settings are unset and spored runs
// with its defaults, as it does on an instance with no tags at all. That is
// the right answer for every case that reaches it: a launch that set none of
// them (it has no object), a persistent Spot request's replacement (EC2 gives
// it none of the original's tags; spored retires it, and that check needs
// spored running), and an instance some other tool launched. Refusing to start
// would stop spored's heartbeat, its TTL and cost enforcement from the tags,
// and the replacement check, which is strictly worse. The one case it is
// wrong for, a tags-only instance from a spawn before the object that is now
// running this spored, is reported (warnTagOnlySettings) and is what `spawn
// upgrade-spored` refuses to create.
//
// When there is a tag but the object cannot be had (not cached, and the fetch
// or the hash check failed), Config.LaunchConfigError says so, spored takes no
// idle action until it is loaded (an unread --active-ports must not let an
// instance in use be stopped), the TTL, cost limit and completion action,
// which are tags, are enforced as ever, and every config refresh tries again.

// launchConfigLoader holds what loading the object needs, and the object once
// loaded: it is read once per boot.
type launchConfigLoader struct {
	s3        launchconfig.GetObjectAPI
	bucket    string
	account   string
	cachePath string

	sha    string
	doc    *launchconfig.Document
	src    launchconfig.Source
	warned map[string]bool
}

// apply sets cfg's launch-fixed settings from the object cfg.LaunchConfigSHA
// names, loading it the first time (cache, then S3), and records the outcome
// in cfg.LaunchConfigSource or cfg.LaunchConfigError.
func (l *launchConfigLoader) apply(ctx context.Context, cfg *Config, tags []types.TagDescription) {
	l.warnTagOnlySettings(cfg, tags)
	sha := cfg.LaunchConfigSHA
	if sha == "" {
		return
	}
	if l.doc == nil || l.sha != sha {
		doc, unknown, src, cacheErr, err := launchconfig.Load(ctx, l.s3, l.bucket, l.account, sha, l.cachePath)
		if err != nil {
			cfg.LaunchConfigError = err.Error()
			log.Printf("ERROR: this instance's launch config object (%s=%s) could not be loaded: %v. "+
				"Running without its settings (notifications, webhooks, active ports/processes, web app, idle CPU, "+
				"metrics and tracing details) and taking no idle action until it loads; TTL, cost limit and "+
				"completion are enforced from the tags. Retrying on the next config refresh.",
				tagprefix.Tag(launchconfig.TagSuffix), sha, err)
			return
		}
		if cacheErr != nil {
			log.Printf("Warning: loaded the launch config object from S3 but could not cache it: %v (it will be fetched again next boot)", cacheErr)
		}
		if len(unknown) > 0 {
			log.Printf("Warning: the launch config object has settings this spored does not know, ignored: %s "+
				"(a newer spawn wrote it)", strings.Join(unknown, ", "))
		}
		log.Printf("Launch config object %s loaded from %s: %d setting(s)", sha, src, len(doc.Settings))
		l.sha, l.doc, l.src = sha, doc, src
	}
	cfg.LaunchConfigSource = string(l.src)
	applyLaunchSettings(cfg, l.doc.Settings)
}

// warnTagOnlySettings logs, once per key, a launch-fixed setting found as a
// tag. spored does not read it there (#1359): the instance was launched by a
// spawn older than the object and has been given this spored since.
func (l *launchConfigLoader) warnTagOnlySettings(cfg *Config, tags []types.TagDescription) {
	if l.warned == nil {
		l.warned = map[string]bool{}
	}
	var found []string
	prefix := tagprefix.Prefix() + ":"
	for _, t := range tags {
		k, ok := strings.CutPrefix(derefString(t.Key), prefix)
		if ok && launchconfig.IsSetting(k) && !l.warned[k] {
			l.warned[k] = true
			found = append(found, k)
		}
	}
	if len(found) > 0 {
		cfg.TagOnlySettings = append(cfg.TagOnlySettings, found...)
		log.Printf("WARNING: this instance carries %s as tags, which this spored (v%s or later) does not read: "+
			"they come from a launch config object (%s), and this instance has none. It was launched by a spawn "+
			"older than v%s; reinstall that spawn's spored (spawn upgrade-spored --version) to have them applied.",
			strings.Join(found, ", "), launchconfig.FirstSporedVersion, tagprefix.Tag(launchconfig.TagSuffix),
			launchconfig.FirstSporedVersion)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// applyLaunchSettings sets cfg from an object's settings. Each value is the
// string its tag carried, parsed here exactly as configFromTags parsed the tag
// before #1359 (TestLaunchConfigObjectIsEquivalentToTheTagPath holds the two
// to the same agent config).
func applyLaunchSettings(cfg *Config, settings map[string]string) {
	for k, v := range settings {
		switch k {
		case launchconfig.NotifyURL:
			cfg.NotifyURL = v
		case launchconfig.NotifyCommand:
			cfg.NotifyCommand = v
		case launchconfig.NotifyPlatform:
			cfg.NotifyPlatform = v
		case launchconfig.SlackWorkspaceID:
			cfg.SlackWorkspaceID = v
		case launchconfig.ActivePorts:
			cfg.ActivePorts = nil
			for _, p := range strings.Split(v, ",") {
				p = strings.TrimSpace(p)
				if port, err := strconv.Atoi(p); err == nil && port > 0 {
					cfg.ActivePorts = append(cfg.ActivePorts, port)
				}
			}
		case launchconfig.ActiveProcesses:
			cfg.ActiveProcesses = nil
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					cfg.ActiveProcesses = append(cfg.ActiveProcesses, p)
				}
			}
		case launchconfig.AppMode:
			cfg.AppMode = v
		case launchconfig.ReadyPort:
			if port, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				cfg.ReadyPort = port
			}
		case launchconfig.ReadyPath:
			cfg.ReadyHealthPath = v
		case launchconfig.WebAuth:
			cfg.WebAuth = v
		case launchconfig.SpotWebhookURL:
			cfg.SpotWebhookURL = v
		case launchconfig.CompletionWebhookURL:
			cfg.CompletionWebhookURL = v
		case launchconfig.WebhookCorrelation:
			cfg.WebhookCorrelation = v
		case launchconfig.WebhookTimeout:
			if d, err := ttlpkg.ParseDuration(v); err == nil {
				cfg.WebhookTimeout = d
			}
		case launchconfig.IdleCPU:
			if cpu, err := strconv.ParseFloat(v, 64); err == nil {
				cfg.IdleCPUPercent = cpu
			}
		case launchconfig.MetricsPort:
			if port, err := strconv.Atoi(v); err == nil && port >= 1024 && port <= 65535 {
				cfg.Observability.Metrics.Port = port
			}
		case launchconfig.MetricsBind:
			cfg.Observability.Metrics.Bind = v
		case launchconfig.MetricsPath:
			cfg.Observability.Metrics.Path = v
		case launchconfig.TracingEnabled:
			cfg.Observability.Tracing.Enabled = v == "true"
		case launchconfig.TracingExporter:
			cfg.Observability.Tracing.Exporter = v
		case launchconfig.TracingSampling:
			if rate, err := strconv.ParseFloat(v, 64); err == nil {
				cfg.Observability.Tracing.SamplingRate = rate
			}
		}
	}
}

// LaunchConfigUnavailable reports whether the instance names a launch config
// object spored has not been able to load. Idle actions wait for it.
func (c *Config) LaunchConfigUnavailable() bool {
	return c != nil && c.LaunchConfigSHA != "" && c.LaunchConfigError != ""
}

// FetchLaunchSettings fetches and verifies the launch config object sha names,
// for a caller that has no cache to read (a Spot replacement reading its
// original's notification settings from the request's spawn:config, #1080).
// It does not cache it.
func (p *EC2Provider) FetchLaunchSettings(ctx context.Context, sha string) (map[string]string, error) {
	if p.launch == nil || p.launch.s3 == nil {
		return nil, fmt.Errorf("no S3 client to fetch the launch config object with")
	}
	body, err := launchconfig.Fetch(ctx, p.launch.s3, p.launch.bucket, p.launch.account, sha)
	if err != nil {
		return nil, err
	}
	doc, _, err := launchconfig.Decode(body, sha)
	if err != nil {
		return nil, err
	}
	return doc.Settings, nil
}
