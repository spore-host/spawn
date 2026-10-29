package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
)

// tagLimitAWS answers every AWS request a launch makes and records its action.
// Only GetCallerIdentity succeeds: runLaunch's best-effort audit lookup, the
// one call it makes before it can know anything about the launch.
type tagLimitAWS struct {
	mu      sync.Mutex
	actions []string
}

func (f *tagLimitAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	action := r.Header.Get("X-Amz-Target")
	if action == "" {
		form, _ := url.ParseQuery(string(body))
		action = form.Get("Action")
	}
	if action == "" {
		action = r.Method + " " + r.URL.Path
	}
	f.mu.Lock()
	f.actions = append(f.actions, action)
	f.mu.Unlock()
	if action == "GetCallerIdentity" {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDATEST</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>t</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
		return
	}
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>not in this fake</Message></Error></Errors><RequestID>t</RequestID></Response>`)
}

// beyondIdentity is every recorded action other than GetCallerIdentity.
func (f *tagLimitAWS) beyondIdentity() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.actions {
		if a != "GetCallerIdentity" {
			out = append(out, a)
		}
	}
	return out
}

// keepLaunchGlobals puts back, when the test ends, every launch flag and the
// globals runLaunch and launchParameterSweep assign themselves (the sweep's
// auto --detach and --max-concurrent, --keep-on-error), which other tests in
// the package read.
func keepLaunchGlobals(t *testing.T) {
	t.Helper()
	type saved struct {
		value   string
		slice   []string
		changed bool
	}
	flags := map[string]saved{}
	launchCmd.Flags().VisitAll(func(f *pflag.Flag) {
		s := saved{value: f.Value.String(), changed: f.Changed}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			s.slice = sv.GetSlice()
		}
		flags[f.Name] = s
	})
	prevKeep, prevWaitSSH, prevCompliance := keepOnError, waitForSSHSet, complianceMode
	t.Cleanup(func() {
		launchCmd.Flags().VisitAll(func(f *pflag.Flag) {
			s := flags[f.Name]
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(s.slice)
			} else {
				_ = f.Value.Set(s.value)
			}
			f.Changed = s.changed
		})
		keepOnError, waitForSSHSet, complianceMode = prevKeep, prevWaitSSH, prevCompliance
	})
}

func serveTagLimitAWS(t *testing.T) *tagLimitAWS {
	t.Helper()
	keepLaunchGlobals(t)
	dir := isolateAWSEnv(t)
	f := &tagLimitAWS{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return f
}

// fiftyTags is one more custom tag than any launch has room for: buildTags
// always writes the spawn:managed baseline on top.
func fiftyTags() []string {
	out := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		out = append(out, fmt.Sprintf("project-%02d=v", i))
	}
	return out
}

// A `spawn launch` over AWS's 50-tag limit is refused before it makes a single
// AWS call beyond runLaunch's audit identity lookup (#1336): no key pair, IAM
// role, security group, Strata lockfile or FSx is created, and nothing is even
// looked up, for a launch RunInstances would reject. It used to be refused by
// RunInstances itself, after all of them.
func TestLaunchOverTheTagLimitMakesNoAWSCall(t *testing.T) {
	f := serveTagLimitAWS(t)
	prev := struct {
		name, instanceType, region, ttl, ami string
		tags                                 []string
		yes                                  bool
	}{name, instanceType, region, ttl, ami, launchTags, autoYes}
	t.Cleanup(func() {
		name, instanceType, region, ttl, ami, launchTags, autoYes =
			prev.name, prev.instanceType, prev.region, prev.ttl, prev.ami, prev.tags, prev.yes
	})
	name, instanceType, region, ttl, ami, launchTags, autoYes =
		"tagged", "m7i.large", "us-east-1", "1h", "", fiftyTags(), true

	err := runLaunch(launchCmd, nil)
	var limit *aws.TagLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("runLaunch with 50 --tag values = %v; want a *aws.TagLimitError", err)
	}
	if !strings.Contains(err.Error(), "50 custom --tag") {
		t.Errorf("the refusal does not say the 50 --tag values are what to drop: %v", err)
	}
	if calls := f.beyondIdentity(); len(calls) != 0 {
		t.Errorf("a launch refused for its tag count made %d AWS call(s) first: %v", len(calls), calls)
	}
}

// A `spawn launch` whose tags fit in 50 but leave no slot for the ones spored
// writes once it runs (#1355) is refused before any AWS call beyond the audit
// identity lookup, saying how many slots spored needs. #1336 allowed it: at 50
// tags every spored write fails, spawn:compute-seconds among them.
func TestLaunchLeavingSporedNoRoomMakesNoAWSCall(t *testing.T) {
	f := serveTagLimitAWS(t)
	prev := struct {
		name, instanceType, region, ttl, ami string
		tags                                 []string
		yes                                  bool
	}{name, instanceType, region, ttl, ami, launchTags, autoYes}
	t.Cleanup(func() {
		name, instanceType, region, ttl, ami, launchTags, autoYes =
			prev.name, prev.instanceType, prev.region, prev.ttl, prev.ami, prev.tags, prev.yes
	})
	name, instanceType, region, ttl, ami, launchTags, autoYes =
		"tagged", "m7i.large", "us-east-1", "1h", "", fiftyTags(), true

	// How many tags the launch writes besides the --tag values, from the
	// refusal of fifty of them.
	var limit *aws.TagLimitError
	if err := runLaunch(launchCmd, nil); !errors.As(err, &limit) {
		t.Fatalf("runLaunch with 50 --tag values = %v; want a *aws.TagLimitError", err)
	}
	reserve := len(limit.Spored) + len(limit.PostLaunch)
	own := limit.Count - reserve - 50
	if own <= 0 {
		t.Fatalf("refusal counts %d tags with %d reserved; cannot size the launch", limit.Count, reserve)
	}

	launchTags = fiftyTags()[:50-own] // exactly 50 tags at launch
	err := runLaunch(launchCmd, nil)
	if !errors.As(err, &limit) {
		t.Fatalf("runLaunch with %d tags at launch and none of the %d reserved slots free = %v; want a *aws.TagLimitError",
			50, reserve, err)
	}
	if want := fmt.Sprintf("%d slot(s) spored needs", len(limit.Spored)); len(limit.Spored) == 0 || !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not say how many slots spored needs (%q): %v", want, err)
	}
	if calls := f.beyondIdentity(); len(calls) != 0 {
		t.Errorf("a launch refused for leaving spored no room made %d AWS call(s) first: %v", len(calls), calls)
	}
}

// A parameter sweep any of whose members is over the limit is refused before
// any AWS call (#1336): before applyCLIIAMToSweep creates a role, and before
// either orchestration path imports a key pair or uploads the params. Here
// every #1308 spored setting on a sweep that also sets most launch options,
// which since #1359 fits on its own (those settings are in the launch config
// object), plus custom tags to take it over.
func TestSweepOverTheTagLimitMakesNoAWSCall(t *testing.T) {
	f := serveTagLimitAWS(t)
	dir := t.TempDir()
	pf := filepath.Join(dir, "sweep.yaml")
	if err := os.WriteFile(pf, []byte("defaults:\n  instance_type: m7i.large\n  ttl: 1h\nparams:\n  - alpha: 1\n  - alpha: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prevFile, prevYes := paramFile, autoYes
	t.Cleanup(func() { paramFile, autoYes = prevFile, prevYes })
	paramFile, autoYes = pf, true

	base := maximalSweepBase()
	base.Tags = map[string]string{}
	for i := 0; i < 20; i++ {
		base.Tags[fmt.Sprintf("team%02d", i)] = "x"
	}
	err := launchParameterSweep(t.Context(), &base, &platform.Platform{OS: "linux", HomeDir: dir}, newAuditLogger("test", "test"))
	var limit *aws.TagLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("launchParameterSweep = %v; want a *aws.TagLimitError", err)
	}
	if !strings.Contains(err.Error(), "20 custom --tag") || !strings.Contains(err.Error(), "--metrics (spawn:metrics-enabled)") {
		t.Errorf("the refusal does not name what to drop: %v", err)
	}
	if calls := f.beyondIdentity(); len(calls) != 0 || len(f.actions) != 0 {
		t.Errorf("a sweep refused for its tag count made AWS call(s) first: %v", f.actions)
	}
}

// maximalSweepBase is a `spawn launch --param-file` base config with the
// options that each add a tag, and every spored setting.
func maximalSweepBase() aws.LaunchConfig {
	sampling := 0.5
	return aws.LaunchConfig{
		InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-0123456789abcdef0", KeyName: "k",
		IamInstanceProfile: "spored-instance-profile",
		DNSName:            "d", DNSDomain: "example.org", DNSEndpoint: "https://dns.example.org",
		TTL: "4h", IdleTimeout: "1h", HibernateOnIdle: true, CostLimit: 10, PricePerHour: 0.085,
		SessionTimeout: "8h", PreStop: "sync", PreStopTimeout: "5m",
		SpotInterruptionWebhookURL: "https://spot", WebhookCorrelation: "corr", WebhookTimeout: "2s",
		CompletionWebhookURL: "https://done", NotifyURL: "https://n", NotifyCommand: "/spore",
		SlackWorkspaceID: "T1", NotifyPlatform: "slack", ActivePortsRaw: "8888", ActiveProcessesRaw: "python",
		EFSID: "fs-1", EFSMountPoint: "/efs", FSxLustreID: "fs-2", FSxMountPoint: "/fsx", FSxMountName: "m",
		OnComplete: "terminate", CompletionFile: "/tmp/C", CompletionDelay: "1m", JobArrayCommand: "run.sh",
		Spot: true, Hibernate: true,
		IdleCPUPercent: 10,
		Observability: aws.SporedObservability{
			MetricsEnabled: true, MetricsPort: 9100, MetricsBind: "0.0.0.0", MetricsPath: "/m",
			TracingEnabled: true, TracingSampling: &sampling,
		},
	}
}

// The pre-flight stands in for what launchWithProgress learns only after it
// has created something: a durable FSx's ID and mount name, the Strata
// lockfile's URI, a job array's tags. A launch that only goes over once those
// are in must still be refused up front.
func TestPreflightLaunchTagsCountsWhatTheLaunchAddsLater(t *testing.T) {
	prevCount, prevStrata, prevOS := count, strataProfile, osFlag
	t.Cleanup(func() { count, strataProfile, osFlag = prevCount, prevStrata, prevOS })
	count, strataProfile, osFlag = 1, "", ""

	cfg := aws.LaunchConfig{Name: "n", InstanceType: "m7i.large", Region: "us-east-1", TTL: "1h", Tags: map[string]string{}}
	fill := func() {
		for i := 0; ; i++ {
			cfg.Tags[fmt.Sprintf("project-%02d", i)] = "v"
			if err := preflightLaunchTags(&cfg); err != nil {
				delete(cfg.Tags, fmt.Sprintf("project-%02d", i))
				return
			}
		}
	}
	fill()
	if err := preflightLaunchTags(&cfg); err != nil {
		t.Fatalf("the filled launch is refused: %v", err)
	}

	for what, set := range map[string]func(){
		"a durable --fsx-create":    func() { cfg.FSxLustreCreate, cfg.FSxLifecycle = true, "durable" },
		"an ephemeral --fsx-create": func() { cfg.FSxLustreCreate, cfg.FSxLifecycle = true, "ephemeral" },
		"--fsx-recall":              func() { cfg.FSxLustreRecall = "fs-0123" },
		"--strata-profile":          func() { strataProfile = "p" },
		"--count 2":                 func() { count = 2 },
	} {
		saved := cfg
		set()
		var limit *aws.TagLimitError
		if err := preflightLaunchTags(&cfg); !errors.As(err, &limit) {
			t.Errorf("%s at the limit: preflightLaunchTags = %v, want a refusal for the tags it adds later", what, err)
		}
		cfg, count, strataProfile = saved, 1, ""
	}

	// Counted exactly, not by a worst case: a durable filesystem adds three
	// tags (ID, mount point, mount name), so with three slots free it fits.
	for i := 0; i < 3; i++ {
		for k := range cfg.Tags {
			delete(cfg.Tags, k)
			break
		}
	}
	cfg.FSxLustreCreate, cfg.FSxLifecycle = true, "durable"
	if err := preflightLaunchTags(&cfg); err != nil {
		t.Errorf("a durable --fsx-create with the three slots it needs was refused: %v", err)
	}
}
