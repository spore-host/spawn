package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/platform"
)

// testDetachedBootstrap is a detachedBootstrap with a local public key in a
// temp home and every member on Linux.
func testDetachedBootstrap(t *testing.T) detachedBootstrap {
	t.Helper()
	dir := t.TempDir()
	pub := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAAdetachedtest spawn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return detachedBootstrap{
		plat: &platform.Platform{OS: "linux", HomeDir: dir, SSHPubKeyPath: pub},
		osOf: func(string, string) string { return "linux" },
		// Launch's own builder, with an identity and price fixed so the test
		// needs no AWS (aws.LaunchTagger supplies both in production).
		tagsOf: testMemberTags,
		// Launch's own resolution, with the AMI and memory lookups fixed.
		settingsOf: testMemberSettings,
		// Launch's own storage rendering, with the FSx and EFS lookups fixed.
		storageOf: func(cfg *aws.LaunchConfig) (string, []string, error) {
			return newSweepStorage(&fakeStorageLookups{}, io.Discard).member(context.Background(), cfg)
		},
		// Launch's own EFA resolution (#1407), with DescribeInstanceTypes
		// fixed: a type the tests do not name has one EFA card, so plain
		// --efa is carried to a detached member rather than refused.
		efaOf: func(cfg *aws.LaunchConfig) error {
			return resolveSweepMemberEFA(cfg, testEFACapacity(1))
		},
		// --no-public-ip's subnet check (#1437), passing, pinning a subnet
		// for an AZ-only member as aws.Client.PreflightNoPublicIP does.
		privateNetOf: func(cfg *aws.LaunchConfig) error {
			if cfg.SubnetID == "" {
				cfg.SubnetID = "subnet-in-" + cfg.AvailabilityZone
			}
			return nil
		},
	}
}

// testMemberTags is what aws.LaunchTagger renders for cfg, with a fixed caller
// and price.
func testMemberTags(cfg aws.LaunchConfig) (map[string]string, error) {
	tags, err := testMemberTagList(cfg)
	if err != nil {
		return nil, err
	}
	return tagsAsMap(tags), nil
}

func testMemberTagList(cfg aws.LaunchConfig) ([]ec2types.Tag, error) {
	cfg.PricePerHour, cfg.PriceSource = 0.0416, "live"
	return aws.BuildLaunchTags(cfg, "111122223333", "arn:aws:iam::111122223333:user/u", "")
}

// decodedScript reverses encodeUserData (gzip, then base64).
func decodedScript(t *testing.T, ud string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(ud)
	if err != nil {
		t.Fatalf("user data is not base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("user data is not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// copyParamFile deep-copies pf's maps, so the foreground build can see the
// file as it was before seeding.
func copyParamFile(pf *ParamFileFormat) *ParamFileFormat {
	cp := &ParamFileFormat{Defaults: map[string]interface{}{}}
	for k, v := range pf.Defaults {
		cp.Defaults[k] = v
	}
	for _, row := range pf.Params {
		r := map[string]interface{}{}
		for k, v := range row {
			r[k] = v
		}
		cp.Params = append(cp.Params, r)
	}
	return cp
}

// memberUserData is the user data the orchestrator will launch member i with:
// its row's own, else the defaults' (mergeParams in the Lambda).
func memberUserData(pf *ParamFileFormat, i int) string {
	if ud := stringParam(pf.Params[i], detachedUserDataKey); ud != "" {
		return ud
	}
	return stringParam(pf.Defaults, detachedUserDataKey)
}

// TestDetachedMemberUserDataIsTheForegroundMembers is #1323's CLI half: the
// user data a detached member launches with is, byte for byte and decoded, what
// the foreground sweep path builds for the same member config
// (buildLaunchConfigFromParams, then buildUserData and encodeUserDataForOS, as
// launchParameterSweep's foreground loop does). The Lambda half,
// TestMemberLaunchesWithTheCLIsBootstrap, proves the Lambda passes it through.
func TestDetachedMemberUserDataIsTheForegroundMembers(t *testing.T) {
	boot := testDetachedBootstrap(t)
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0", "region": "us-east-1", "ttl": "2h"},
		Params: []map[string]interface{}{
			{"trial": "a"},
			{"trial": "b", "completion_file": "/data/done"}, // a member-specific bootstrap input
		},
	}
	before := copyParamFile(pf)
	base := aws.LaunchConfig{InstanceType: "t3.micro"}

	if err := seedDetachedBootstrap(pf, base, "sw-1", "sw", boot); err != nil {
		t.Fatalf("seedDetachedBootstrap: %v", err)
	}

	for i := range pf.Params {
		cfg, err := buildLaunchConfigFromParams(base, before.Defaults, before.Params[i], "sw-1", "sw", i, len(before.Params))
		if err != nil {
			t.Fatal(err)
		}
		cfg.TargetOS = "linux"
		script, err := buildUserData(boot.plat, &cfg, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want := encodeUserDataForOS(script, cfg.TargetOS)

		got := memberUserData(pf, i)
		if got == "" {
			t.Fatalf("member %d carries no spored user data: the orchestrator would refuse it (and before #1323 launched it with none)", i)
		}
		if got != want {
			t.Errorf("member %d's user data differs from the foreground path's for the same config", i)
		}
		if decodedScript(t, got) != script {
			t.Errorf("member %d's decoded user data is not the foreground script", i)
		}
	}
	if !strings.Contains(decodedScript(t, memberUserData(pf, 1)), "SPAWN_CMD_COMPLETION_FILE='/data/done'") {
		t.Error("member 1's own completion_file did not reach its bootstrap")
	}
	if memberUserData(pf, 0) == memberUserData(pf, 1) {
		t.Error("two members with different bootstrap inputs got the same user data")
	}
}

// The stub names the release that built it; a dev build inlines the body, as
// every other path does (#1274).
func TestDetachedBootstrapIsTheReleaseStubOrDevInline(t *testing.T) {
	prev := launcher.CallerVersion
	t.Cleanup(func() { launcher.CallerVersion = prev })

	seed := func() string {
		pf := &ParamFileFormat{
			Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-1", "region": "us-east-1"},
			Params:   []map[string]interface{}{{"trial": "a"}},
		}
		if err := seedDetachedBootstrap(pf, aws.LaunchConfig{}, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
			t.Fatal(err)
		}
		return decodedScript(t, memberUserData(pf, 0))
	}

	launcher.CallerVersion = "0.147.0"
	if s := seed(); !strings.Contains(s, "SPAWN_BS_VERSION='0.147.0'") || strings.Contains(s, "systemctl start spored") {
		t.Error("a release CLI's detached member does not fetch that release's published bootstrap body")
	}
	launcher.CallerVersion = "dev"
	if s := seed(); strings.Contains(s, "SPAWN_BS_VERSION=") || !strings.Contains(s, "systemctl start spored") {
		t.Error("a dev CLI's detached member does not carry the bootstrap body inline")
	}
}

// Members that agree share one copy under defaults:, and a value the file
// itself wrote under the key is replaced, never booted.
func TestDetachedBootstrapWrittenOnceAndNeverFromTheFile(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-1", "region": "us-east-1", detachedUserDataKey: "evil"},
		Params:   []map[string]interface{}{{"trial": "a", detachedUserDataKey: "evil-row"}, {"trial": "b"}},
	}
	if err := seedDetachedBootstrap(pf, aws.LaunchConfig{}, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
		t.Fatal(err)
	}
	ud := stringParam(pf.Defaults, detachedUserDataKey)
	if ud == "" || ud == "evil" {
		t.Fatalf("defaults user data = %q, want spawn's bootstrap", ud)
	}
	for i, row := range pf.Params {
		if _, ok := row[detachedUserDataKey]; ok {
			t.Errorf("row %d still carries its own %s although every member agrees", i, detachedUserDataKey)
		}
	}
	if !strings.Contains(decodedScript(t, ud), "LOCAL_USERNAME=") {
		t.Error("the shared user data is not a spored bootstrap")
	}

	// And the key is refused up front, before anything is seeded.
	file := &ParamFileFormat{Defaults: map[string]interface{}{detachedUserDataKey: "x"}, Params: []map[string]interface{}{{}}}
	if err := validateSweepParamKeys(file); err == nil {
		t.Errorf("a param file may set %s", detachedUserDataKey)
	}
}

// With no key pair and no local public key the member is keyless (SSM), not
// a failed sweep.
func TestDetachedBootstrapKeylessWithoutALocalKey(t *testing.T) {
	boot := testDetachedBootstrap(t)
	boot.plat.SSHPubKeyPath = filepath.Join(t.TempDir(), "absent.pub")
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-1", "region": "us-east-1"},
		Params:   []map[string]interface{}{{"trial": "a"}},
	}
	if err := seedDetachedBootstrap(pf, aws.LaunchConfig{}, "sw-1", "sw", boot); err != nil {
		t.Fatalf("a keyless detached sweep failed: %v", err)
	}
	if !strings.Contains(decodedScript(t, memberUserData(pf, 0)), "LOCAL_SSH_KEY_BASE64=''") {
		t.Error("the keyless member's bootstrap installs a key")
	}
}

type fakeProfileEnsurer struct{ calls int }

func (f *fakeProfileEnsurer) SetupSporedIAMRole(context.Context) (string, error) {
	f.calls++
	return aws.SporedInstanceProfileName, nil
}

// A member with no iam_role gets spored's profile, created by spawn; a sweep
// whose members all name one is left alone.
func TestEnsureDetachedInstanceProfile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pf        *ParamFileFormat
		wantCalls int
		want      string
	}{
		{"none", &ParamFileFormat{Params: []map[string]interface{}{{"a": 1}}}, 1, aws.SporedInstanceProfileName},
		{"sweep-wide", &ParamFileFormat{Defaults: map[string]interface{}{"iam_role": "mine"}, Params: []map[string]interface{}{{}}}, 0, "mine"},
		{"every row", &ParamFileFormat{Defaults: map[string]interface{}{}, Params: []map[string]interface{}{{"iam_role": "r"}}}, 0, ""},
		{"some rows", &ParamFileFormat{Defaults: map[string]interface{}{}, Params: []map[string]interface{}{{"iam_role": "r"}, {}}}, 1, aws.SporedInstanceProfileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &fakeProfileEnsurer{}
			if err := ensureDetachedInstanceProfile(context.Background(), e, tc.pf); err != nil {
				t.Fatal(err)
			}
			if e.calls != tc.wantCalls || stringParam(tc.pf.Defaults, "iam_role") != tc.want {
				t.Errorf("calls=%d iam_role=%q, want %d and %q", e.calls, stringParam(tc.pf.Defaults, "iam_role"), tc.wantCalls, tc.want)
			}
		})
	}
}

// A schedule's members launch through the same orchestrator, so what
// `spawn schedule create` stages carries the bootstrap and the profile too;
// the schedule's own defaults: settings do not break the member build.
func TestSeedScheduleParams_CarriesTheBootstrap(t *testing.T) {
	p := &parsedParams{
		defaults: map[string]interface{}{"instance_type": "t3.micro", "max_concurrent": 5, "sweep_name": "nightly", "launch_delay": "5s"},
		params:   []map[string]interface{}{{"trial": "a", "ami": "ami-row"}},
	}
	e := &fakeProfileEnsurer{}
	data, err := seedScheduleParams(context.Background(), nil, p, "eu-west-1", e, testDetachedBootstrap(t))
	if err != nil {
		t.Fatalf("seedScheduleParams: %v", err)
	}
	var got ParamFileFormat
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if ud := memberUserData(&got, 0); ud == "" || !strings.Contains(decodedScript(t, ud), "LOCAL_USERNAME=") {
		t.Error("a scheduled sweep's member carries no spored bootstrap; the orchestrator refuses it")
	}
	if got.Defaults["iam_role"] != aws.SporedInstanceProfileName || e.calls != 1 {
		t.Errorf("iam_role = %v (ensure calls %d), want spawn's spored profile", got.Defaults["iam_role"], e.calls)
	}
	if got.Defaults["max_concurrent"] != 5.0 {
		t.Errorf("the schedule's own max_concurrent was lost: %v", got.Defaults["max_concurrent"])
	}
}

// launchSweepDetached seeds the bootstrap and the profile before it uploads
// the params the orchestrator launches from; seeding after the upload, or not
// at all, ships members the orchestrator refuses.
func TestLaunchSweepDetachedSeedsBeforeUpload(t *testing.T) {
	src, err := os.ReadFile("launch_sweep.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "\nfunc launchSweepDetached(")
	if start < 0 {
		t.Fatal("cannot find launchSweepDetached")
	}
	body := s[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	at := func(re string) int {
		loc := regexp.MustCompile(`(?m)^\s*` + re).FindStringIndex(body)
		if loc == nil {
			return -1
		}
		return loc[0]
	}
	upload := at(`s3Key, err := sweep\.UploadParamsToS3\(`)
	boot := at(`if err := seedDetachedBootstrap\(paramFormat, \*baseConfig, sweepID, sweepName, `)
	profile := at(`if err := ensureDetachedInstanceProfile\(ctx, regionClient, paramFormat\)`)
	if upload < 0 {
		t.Fatal("cannot find the params upload; the matcher is stale")
	}
	if boot < 0 || boot > upload {
		t.Error("launchSweepDetached does not render the members' spored bootstrap before uploading the params (#1323)")
	}
	if profile < 0 || profile > upload {
		t.Error("launchSweepDetached does not ensure spored's instance profile before uploading the params (#1323)")
	}
}
