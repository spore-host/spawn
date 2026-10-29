package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/launchconfig"
)

// fakeConfigS3 serves launch config objects by key and counts the fetches.
type fakeConfigS3 struct {
	objects map[string][]byte
	gets    int
	owners  []string
}

func (f *fakeConfigS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.gets++
	f.owners = append(f.owners, aws.ToString(in.ExpectedBucketOwner))
	b, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "no such key"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

// goldenV0149 is the tags v0.149.0's launcher wrote for a launch setting every
// launch-fixed setting (pkg/aws/testdata, captured before #1359).
func goldenV0149(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "aws", "testdata", "every-setting-tags.v0.149.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func descsOf(m map[string]string) []ec2types.TagDescription {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []ec2types.TagDescription
	for _, k := range keys {
		out = append(out, ec2types.TagDescription{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

// splitGolden is the golden launch as this spawn writes it: the tags that stay
// tags plus spawn:config, and the object holding the rest. pkg/aws's
// TestLaunchConfigObjectHoldsWhatTheTagsHeld proves the launcher writes
// exactly this split for the same launch.
func splitGolden(t *testing.T, golden map[string]string) (tags map[string]string, body []byte, sha string) {
	t.Helper()
	tags = map[string]string{}
	settings := map[string]string{}
	for k, v := range golden {
		if launchconfig.IsSetting(k) {
			settings[strings.TrimPrefix(k, "spawn:")] = v
		} else {
			tags[k] = v
		}
	}
	if len(settings) != len(launchconfig.Settings) {
		t.Fatalf("the golden holds %d of the %d launch-fixed settings", len(settings), len(launchconfig.Settings))
	}
	body, sha, err := launchconfig.Encode(settings)
	if err != nil {
		t.Fatal(err)
	}
	tags[launchconfig.Tag] = sha
	return tags, body, sha
}

// withoutObjectBookkeeping is c with the fields that only say where the
// settings came from cleared, so two configs compare on the settings alone.
func withoutObjectBookkeeping(c Config) Config {
	c.LaunchConfigSHA, c.LaunchConfigSource, c.LaunchConfigError, c.TagOnlySettings = "", "", "", nil
	c.Plugins = nil
	return c
}

// The round trip, spored side (#1359): given the tags and the cached object
// this spawn writes for a launch, spored runs with exactly the agent config
// v0.149.0's spored derived from that launch's v0.149.0 tags, field for field.
// The oracle is v0.149.0's configFromTags, kept verbatim
// (legacyConfigFromTagsV0149).
func TestLaunchConfigObjectIsEquivalentToTheTagPath(t *testing.T) {
	golden := goldenV0149(t)
	want, wantName := legacyConfigFromTagsV0149(descsOf(golden))

	tags, body, sha := splitGolden(t, golden)
	cache := filepath.Join(t.TempDir(), "launch-config.json")
	if err := launchconfig.WriteCache(cache, body); err != nil {
		t.Fatal(err)
	}
	descs := descsOf(tags)
	got, gotName := configFromTags(descs)
	(&launchConfigLoader{cachePath: cache}).apply(context.Background(), got, descs)

	if got.LaunchConfigSHA != sha || got.LaunchConfigError != "" || got.LaunchConfigSource != string(launchconfig.FromCache) {
		t.Fatalf("object %q from %q, error %q; want %s from the cache", got.LaunchConfigSHA, got.LaunchConfigSource, got.LaunchConfigError, sha)
	}
	if gotName != wantName {
		t.Errorf("instance name %q, want %q", gotName, wantName)
	}
	g, w := withoutObjectBookkeeping(*got), withoutObjectBookkeeping(*want)
	if !reflect.DeepEqual(g, w) {
		gv, wv := reflect.ValueOf(g), reflect.ValueOf(w)
		for i := 0; i < gv.NumField(); i++ {
			if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
				t.Errorf("%s: v0.149.0's tag path gave %#v, tags+object give %#v",
					gv.Type().Field(i).Name, wv.Field(i).Interface(), gv.Field(i).Interface())
			}
		}
	}
	// And the golden really exercises the moved settings: a launch with none
	// would pass the comparison vacuously.
	if want.NotifyURL == "" || want.SpotWebhookURL == "" || len(want.ActivePorts) == 0 || want.IdleCPUPercent == 5 ||
		!want.Observability.Tracing.Enabled || want.Observability.Metrics.Port == 0 || want.ReadyPort == 0 {
		t.Fatalf("the golden launch does not set the launch-fixed settings: %+v", want)
	}
}

// No tag read for a moved key: a launch-fixed setting present as a tag (an
// instance launched by a spawn before the object, now running this spored) is
// not applied, and is reported, with no spawn:config the settings stay unset.
// This is the one path where this spored meets a tags-only instance; `spawn
// upgrade-spored` refuses to create it.
func TestATagOnlySettingIsReportedNotRead(t *testing.T) {
	golden := goldenV0149(t)
	descs := descsOf(golden)
	cfg, _ := configFromTags(descs)
	(&launchConfigLoader{cachePath: filepath.Join(t.TempDir(), "none.json")}).apply(context.Background(), cfg, descs)
	if cfg.NotifyURL != "" || cfg.SpotWebhookURL != "" || cfg.IdleCPUPercent != 5 || len(cfg.ActivePorts) != 0 {
		t.Errorf("a launch-fixed setting was read from a tag: notify %q, webhook %q, idle cpu %v, ports %v",
			cfg.NotifyURL, cfg.SpotWebhookURL, cfg.IdleCPUPercent, cfg.ActivePorts)
	}
	if len(cfg.TagOnlySettings) != len(launchconfig.Settings) {
		t.Errorf("reported %d tag-only settings (%v), want all %d", len(cfg.TagOnlySettings), cfg.TagOnlySettings, len(launchconfig.Settings))
	}
	// The settings that stay tags are still read from them.
	if cfg.TTL != 4*time.Hour || cfg.OnComplete != "terminate" || cfg.CostLimit != 10 || !cfg.Observability.Metrics.Enabled {
		t.Errorf("a setting that stays a tag was lost: ttl %v, on-complete %q, cost limit %v, metrics %v",
			cfg.TTL, cfg.OnComplete, cfg.CostLimit, cfg.Observability.Metrics.Enabled)
	}
}

// Content addressing: a cached object that does not hash to the instance's
// spawn:config is never read. It is discarded and the object fetched again;
// one that S3 serves tampered is refused too, and then spored has no settings
// from it, says why, and takes no idle action.
func TestATamperedLaunchConfigIsRefused(t *testing.T) {
	_, body, sha := splitGolden(t, goldenV0149(t))
	tags := descsOf(map[string]string{launchconfig.Tag: sha, "spawn:managed": "true"})
	tampered := bytes.Replace(body, []byte("https://notify.example"), []byte("https://evil.example"), 1)
	if bytes.Equal(tampered, body) {
		t.Fatal("the tamper changed nothing")
	}

	t.Run("cache tampered, S3 intact", func(t *testing.T) {
		cache := filepath.Join(t.TempDir(), "launch-config.json")
		if err := os.WriteFile(cache, tampered, 0o600); err != nil {
			t.Fatal(err)
		}
		f := &fakeConfigS3{objects: map[string][]byte{launchconfig.Key(sha): body}}
		cfg, _ := configFromTags(tags)
		(&launchConfigLoader{s3: f, bucket: "b", account: "123456789012", cachePath: cache}).apply(context.Background(), cfg, tags)
		if cfg.LaunchConfigError != "" || cfg.NotifyURL != "https://notify.example/u?token=n" || cfg.LaunchConfigSource != string(launchconfig.FromS3) {
			t.Fatalf("notify %q from %q, error %q; want the intact object from S3", cfg.NotifyURL, cfg.LaunchConfigSource, cfg.LaunchConfigError)
		}
		if f.gets != 1 || f.owners[0] != "123456789012" {
			t.Errorf("fetched %d time(s) with owners %v, want once, as the account", f.gets, f.owners)
		}
		if b, _ := os.ReadFile(cache); !bytes.Equal(b, body) {
			t.Error("the tampered cache was not replaced by the verified object")
		}
		if fi, err := os.Stat(cache); err == nil && fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
			t.Errorf("cache mode %v, want 0600", fi.Mode().Perm())
		}
	})
	t.Run("both tampered", func(t *testing.T) {
		cache := filepath.Join(t.TempDir(), "launch-config.json")
		_ = os.WriteFile(cache, tampered, 0o600)
		f := &fakeConfigS3{objects: map[string][]byte{launchconfig.Key(sha): tampered}}
		cfg, _ := configFromTags(tags)
		(&launchConfigLoader{s3: f, bucket: "b", cachePath: cache}).apply(context.Background(), cfg, tags)
		if cfg.NotifyURL != "" || !cfg.LaunchConfigUnavailable() || !strings.Contains(cfg.LaunchConfigError, "does not hash") {
			t.Fatalf("notify %q, error %q; want nothing read from a tampered object, and the reason", cfg.NotifyURL, cfg.LaunchConfigError)
		}
		if _, err := os.Stat(cache); err == nil {
			t.Error("a tampered object was cached")
		}
	})
}

// The object is read once per boot (#1359): the first boot fetches and
// caches it, a refresh (spored reload, the tick) reuses it, and the next boot
// reads the cache. Counted through the S3 seam.
func TestTheLaunchConfigIsFetchedOnceAndCached(t *testing.T) {
	_, body, sha := splitGolden(t, goldenV0149(t))
	tags := descsOf(map[string]string{launchconfig.Tag: sha})
	cache := filepath.Join(t.TempDir(), "spawn", "launch-config.json")
	f := &fakeConfigS3{objects: map[string][]byte{launchconfig.Key(sha): body}}

	boot := &launchConfigLoader{s3: f, bucket: "b", cachePath: cache}
	for i := 0; i < 3; i++ { // first load, then two refreshes
		cfg, _ := configFromTags(tags)
		boot.apply(context.Background(), cfg, tags)
		if cfg.NotifyURL == "" {
			t.Fatalf("refresh %d lost the settings", i)
		}
	}
	if f.gets != 1 {
		t.Errorf("one boot fetched the object %d times, want 1", f.gets)
	}

	reboot := &launchConfigLoader{s3: f, bucket: "b", cachePath: cache}
	cfg, _ := configFromTags(tags)
	reboot.apply(context.Background(), cfg, tags)
	if f.gets != 1 || cfg.LaunchConfigSource != string(launchconfig.FromCache) {
		t.Errorf("a reboot fetched again (%d fetches, source %q); it must read the cache", f.gets, cfg.LaunchConfigSource)
	}
}

// An object that cannot be fetched is retried on the next refresh, and loads
// then.
func TestAnUnavailableLaunchConfigIsRetried(t *testing.T) {
	_, body, sha := splitGolden(t, goldenV0149(t))
	tags := descsOf(map[string]string{launchconfig.Tag: sha})
	f := &fakeConfigS3{objects: map[string][]byte{}}
	l := &launchConfigLoader{s3: f, bucket: "b", cachePath: filepath.Join(t.TempDir(), "c.json")}
	cfg, _ := configFromTags(tags)
	l.apply(context.Background(), cfg, tags)
	if !cfg.LaunchConfigUnavailable() {
		t.Fatal("a missing object did not mark the config unavailable")
	}
	f.objects[launchconfig.Key(sha)] = body
	cfg, _ = configFromTags(tags)
	l.apply(context.Background(), cfg, tags)
	if cfg.LaunchConfigUnavailable() || cfg.NotifyURL == "" {
		t.Errorf("the retry did not load it: %q", cfg.LaunchConfigError)
	}
}

// A duration in the object takes the shared parser's units, as its tag did
// (#990).
func TestLaunchSettingsDayUnits(t *testing.T) {
	var c Config
	applyLaunchSettings(&c, map[string]string{launchconfig.WebhookTimeout: "2s"})
	if c.WebhookTimeout != 2*time.Second {
		t.Errorf("webhook-timeout 2s = %v", c.WebhookTimeout)
	}
}

// Every launch-fixed setting has a case in applyLaunchSettings: one without
// would be written by the launcher and read by nothing.
func TestEverySettingIsApplied(t *testing.T) {
	values := map[string]string{
		launchconfig.ActivePorts: "8080", launchconfig.ActiveProcesses: "p", launchconfig.ReadyPort: "8081",
		launchconfig.WebhookTimeout: "3s", launchconfig.IdleCPU: "7", launchconfig.MetricsPort: "9100",
		launchconfig.TracingEnabled: "true", launchconfig.TracingSampling: "0.5",
	}
	for _, k := range launchconfig.SettingKeys() {
		v := values[k]
		if v == "" {
			v = "x-" + k
		}
		var base, c Config
		base.IdleCPUPercent, c.IdleCPUPercent = 5, 5
		applyLaunchSettings(&c, map[string]string{k: v})
		if reflect.DeepEqual(c, base) {
			t.Errorf("%s=%q changed nothing in spored's config", k, v)
		}
	}
}
