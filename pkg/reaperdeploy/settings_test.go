package reaperdeploy

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/reaperconfig"
)

const secretURL = "https://hooks.example.invalid/services/T000/B000/s3cr3t-token"

func redeploy(t *testing.T, existing map[string]string, opts Options) (*fakeLambda, *Result, error) {
	t.Helper()
	lamF := &fakeLambda{exists: true, env: existing}
	d := newTestDeployer(&fakeIAM{roleExists: true}, lamF, &fakeEvents{}, &fakeS3{bucketExists: true})
	d.UpdatePoll = -1
	if opts.AccountID == "" {
		opts.AccountID = "1"
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	opts.Version, opts.Artifact = "0.136.0", "/tmp/x.zip"
	res, err := d.Deploy(context.Background(), opts)
	return lamF, res, err
}

// TestRedeployKeepsSettings is #1154: a re-deploy carried over only
// REAPER_DRY_RUN and replaced the rest of the environment, so an upgrade wiped
// REAPER_SPOT_REQUESTS=reap, a raised REAPER_MAX_AGE, the notify URL and any
// other hand-set value, and reset the regions to the deploy region.
func TestRedeployKeepsSettings(t *testing.T) {
	existing := map[string]string{
		"REAPER_SCAN_SELF":     "true",
		"REAPER_DRY_RUN":       "false",
		"REAPER_REGIONS":       "us-east-1,eu-west-1,ap-south-1",
		"REAPER_SPOT_REQUESTS": "reap",
		"REAPER_NET_RESOURCES": "report",
		"REAPER_MAX_AGE":       "336h",
		"REAPER_NOTIFY_URL":    secretURL,
		"REAPER_GRACEFUL":      "true", // set by hand: no flag exists for it
	}
	// The CLI passes the deploy region as the default and no --regions.
	lamF, _, err := redeploy(t, existing, Options{DefaultRegions: "us-east-1"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for k, v := range existing {
		if lamF.lastEnv[k] != v {
			t.Errorf("re-deploy changed %s: was %q, now %q", k, v, lamF.lastEnv[k])
		}
	}
}

// Explicit settings win over the existing environment, and everything not
// named is still kept.
func TestRedeployExplicitSettingsWin(t *testing.T) {
	existing := map[string]string{
		"REAPER_SCAN_SELF":     "true",
		"REAPER_DRY_RUN":       "false",
		"REAPER_REGIONS":       "us-east-1,eu-west-1",
		"REAPER_SPOT_REQUESTS": "report",
		"REAPER_MAX_AGE":       "336h",
		"REAPER_NOTIFY_URL":    secretURL,
		"REAPER_DNS_ZONE_ID":   "Z123",
	}
	lamF, res, err := redeploy(t, existing, Options{
		Regions:        "us-west-2",
		DefaultRegions: "us-east-1",
		Settings: map[string]string{
			reaperconfig.EnvSpotRequests: "reap",
			reaperconfig.EnvNetResources: "off",
			reaperconfig.EnvMaxAge:       "", // remove: back to the reaper's default
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	want := map[string]string{
		"REAPER_SCAN_SELF":     "true",
		"REAPER_DRY_RUN":       "false",
		"REAPER_REGIONS":       "us-west-2",
		"REAPER_SPOT_REQUESTS": "reap",
		"REAPER_NET_RESOURCES": "off",
		"REAPER_NOTIFY_URL":    secretURL,
		"REAPER_DNS_ZONE_ID":   "Z123",
	}
	for k, v := range want {
		if lamF.lastEnv[k] != v {
			t.Errorf("%s = %q, want %q", k, lamF.lastEnv[k], v)
		}
	}
	if v, ok := lamF.lastEnv["REAPER_MAX_AGE"]; ok {
		t.Errorf("an explicit empty --max-age must remove REAPER_MAX_AGE, got %q", v)
	}
	if !strings.Contains(strings.Join(res.Actions, "\n"), "set REAPER_SPOT_REQUESTS=reap") {
		t.Errorf("the deploy report should say what it set: %v", res.Actions)
	}
}

// A fresh deploy gets the deploy region and lands unarmed, settings applied.
func TestFreshDeployAppliesSettingsAndDefaultRegion(t *testing.T) {
	lamF := &fakeLambda{}
	d := newTestDeployer(&fakeIAM{}, lamF, &fakeEvents{}, &fakeS3{})
	res, err := d.Deploy(context.Background(), Options{
		AccountID: "1", Region: "us-east-1", Version: "0.136.0", Artifact: "/tmp/x.zip",
		DefaultRegions: "us-east-1",
		Settings:       map[string]string{reaperconfig.EnvSpotRequests: "reap"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !res.DryRun || lamF.lastEnv["REAPER_DRY_RUN"] != "true" {
		t.Errorf("a fresh deploy must land unarmed: %v", lamF.lastEnv)
	}
	if lamF.lastEnv["REAPER_REGIONS"] != "us-east-1" || lamF.lastEnv["REAPER_SPOT_REQUESTS"] != "reap" {
		t.Errorf("env = %v", lamF.lastEnv)
	}
}

// A setting the reaper would not understand is refused before anything is
// touched, and a rejected notify URL is not echoed.
func TestDeployRejectsInvalidSettingsUpFront(t *testing.T) {
	for name, settings := range map[string]map[string]string{
		"typo mode":    {reaperconfig.EnvSpotRequests: "reep"},
		"bad max age":  {reaperconfig.EnvMaxAge: "forever"},
		"zero age":     {reaperconfig.EnvMaxAge: "0"},
		"bad url":      {reaperconfig.EnvNotifyURL: "ftp://s3cr3t-token@host/x"},
		"not settable": {"REAPER_DRY_RUN": "false"},
	} {
		t.Run(name, func(t *testing.T) {
			lamF := &fakeLambda{exists: true}
			d := newTestDeployer(&fakeIAM{roleExists: true}, lamF, &fakeEvents{}, &fakeS3{bucketExists: true})
			_, err := d.Deploy(context.Background(), Options{
				AccountID: "1", Region: "us-east-1", Version: "0.136.0", Artifact: "/tmp/x.zip", Settings: settings,
			})
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("error leaks the secret: %v", err)
			}
			if len(lamF.calls) != 0 {
				t.Errorf("nothing should be touched before validation, Lambda saw %v", lamF.calls)
			}
		})
	}
}

// The notify URL is a secret: the deploy report names it but never shows it.
func TestDeployNeverPrintsTheNotifyURL(t *testing.T) {
	_, res, err := redeploy(t, map[string]string{"REAPER_DRY_RUN": "true"}, Options{
		Settings: map[string]string{reaperconfig.EnvNotifyURL: secretURL},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	joined := strings.Join(res.Actions, "\n")
	if strings.Contains(joined, "s3cr3t") || strings.Contains(joined, "hooks.example") {
		t.Errorf("deploy report shows the notify URL:\n%s", joined)
	}
	if !strings.Contains(joined, "REAPER_NOTIFY_URL") {
		t.Errorf("deploy report should still say the notify URL was set:\n%s", joined)
	}
}

var reaperGetenv = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("(REAPER_[A-Z0-9_]+)"\)`)

// discoverReaperEnv returns every REAPER_* variable the reaper reads, from its
// source. Discovered, not listed, so a variable added to the reaper later is
// covered without anyone remembering this test.
func discoverReaperEnv(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "lambda", "ttl-reaper", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reaperGetenv.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	// A discovery that found nothing has broken, not proven the reaper clean.
	if len(out) < 5 {
		t.Fatalf("found only %v REAPER_* reads in lambda/ttl-reaper — the discovery is broken", out)
	}
	return out
}

// TestEveryReaperSettingSurvivesARedeploy: every REAPER_* the reaper reads is
// either preserved across a re-deploy, settable by one, or one of the few
// values the deploy deliberately forces.
func TestEveryReaperSettingSurvivesARedeploy(t *testing.T) {
	vars := discoverReaperEnv(t)

	existing := map[string]string{}
	for _, k := range vars {
		existing[k] = "sentinel-" + k
	}
	forced := ForcedEnv()
	for k, v := range forced {
		existing[k] = v
	}
	lamF, _, err := redeploy(t, existing, Options{DefaultRegions: "us-east-1"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for _, k := range vars {
		if lamF.lastEnv[k] != existing[k] {
			t.Errorf("%s read by the reaper was not preserved by a re-deploy: was %q, now %q",
				k, existing[k], lamF.lastEnv[k])
		}
	}

	// And each settable one actually lands when set.
	settable := map[string]bool{}
	for _, k := range reaperconfig.Settable() {
		settable[k] = true
	}
	valid := map[string]string{
		reaperconfig.EnvSpotRequests: "reap",
		reaperconfig.EnvNetResources: "off",
		reaperconfig.EnvMaxAge:       "14d",
		reaperconfig.EnvNotifyURL:    secretURL,
	}
	for k := range settable {
		v, ok := valid[k]
		if !ok {
			t.Errorf("settable %s has no valid example in this test", k)
			continue
		}
		lamF, _, err := redeploy(t, existing, Options{Settings: map[string]string{k: v}})
		if err != nil {
			t.Fatalf("Deploy setting %s: %v", k, err)
		}
		if lamF.lastEnv[k] != v {
			t.Errorf("setting %s did not land: %q", k, lamF.lastEnv[k])
		}
	}
	// Every settable variable must be one the reaper actually reads.
	read := map[string]bool{}
	for _, k := range vars {
		read[k] = true
	}
	for k := range settable {
		if !read[k] {
			t.Errorf("%s is settable by spawn reaper deploy but the reaper never reads it", k)
		}
	}
}
