package cmd

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launchconfig"
)

// detachedObjectBase is a CLI launch config setting launch-fixed settings
// (#1359) that every member of a sweep inherits.
func detachedObjectBase() aws.LaunchConfig {
	return aws.LaunchConfig{
		InstanceType: "t3.micro", NotifyURL: "https://notify.example/u?token=n", SlackWorkspaceID: "T1",
		CompletionWebhookURL: "https://done.example/h", ActivePortsRaw: "8888",
	}
}

func detachedObjectParams() *ParamFileFormat {
	return &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0", "region": "us-east-1", "ttl": "2h"},
		Params:   []map[string]interface{}{{"trial": "a"}, {"trial": "b"}},
	}
}

var seededObject = regexp.MustCompile(`echo '([A-Za-z0-9+/=]+)' \| base64 -d > ` + regexp.QuoteMeta(launchconfig.LinuxCachePath))

// A detached member is launched by the sweep-orchestrator Lambda, which
// cannot store a launch config object in the member's account, possibly days
// after the CLI rendered it (#1359). So its user data seeds the object into
// spored's cache, as the exact bytes its spawn:config tag hashes, and its
// tags carry none of the settings.
func TestDetachedMemberCarriesItsLaunchConfigObject(t *testing.T) {
	boot := testDetachedBootstrap(t)
	pf := detachedObjectParams()
	if err := seedDetachedBootstrap(pf, detachedObjectBase(), "sw-1", "sw", boot); err != nil {
		t.Fatalf("seedDetachedBootstrap: %v", err)
	}
	for i, row := range pf.Params {
		tags, _ := row[detachedTagsKey].(map[string]interface{})
		sha, _ := tags[launchconfig.Tag].(string)
		if !launchconfig.ValidSHA(sha) {
			t.Fatalf("member %d has no %s tag: %v", i, launchconfig.Tag, tags)
		}
		for k := range tags {
			if launchconfig.IsSetting(k) {
				t.Errorf("member %d carries %s as a tag", i, k)
			}
		}
		script := decodedScript(t, memberUserData(pf, i))
		m := seededObject.FindStringSubmatch(script)
		if m == nil {
			t.Fatalf("member %d's user data does not seed %s", i, launchconfig.LinuxCachePath)
		}
		body, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			t.Fatal(err)
		}
		doc, _, err := launchconfig.Decode(body, sha)
		if err != nil {
			t.Fatalf("member %d: the seeded object is not the one its tag names: %v", i, err)
		}
		if doc.Settings[launchconfig.NotifyURL] != "https://notify.example/u?token=n" || doc.Settings[launchconfig.ActivePorts] != "8888" {
			t.Errorf("member %d's object = %v", i, doc.Settings)
		}
		if strings.Contains(script, "token=n") {
			t.Errorf("member %d's user data carries the notify URL in clear", i)
		}
		if !strings.Contains(script, "chmod 600 "+launchconfig.LinuxCachePath) {
			t.Errorf("member %d's seeded cache is not made 0600", i)
		}
	}
}

// The seed is the only per-launch growth in a detached member's user data;
// report it, and hold it inside EC2's limit.
func TestDetachedLaunchConfigSeedFitsTheUserDataBudget(t *testing.T) {
	boot := testDetachedBootstrap(t)
	plain, seeded := detachedObjectParams(), detachedObjectParams()
	base := detachedObjectBase()
	none := aws.LaunchConfig{InstanceType: base.InstanceType}
	if err := seedDetachedBootstrap(plain, none, "sw-1", "sw", boot); err != nil {
		t.Fatal(err)
	}
	if err := seedDetachedBootstrap(seeded, base, "sw-1", "sw", boot); err != nil {
		t.Fatal(err)
	}
	before, _ := aws.UserDataSize(memberUserData(plain, 0))
	after, _ := aws.UserDataSize(memberUserData(seeded, 0))
	t.Logf("detached member user data: %d bytes without a launch config object, %d with one (+%d) of %d",
		before, after, after-before, aws.MaxUserDataBytes)
	if after <= before {
		t.Errorf("the seeded user data (%d bytes) is not larger than the plain one (%d): nothing was seeded", after, before)
	}
	if err := aws.CheckUserDataSize(memberUserData(seeded, 0)); err != nil {
		t.Error(err)
	}
}

// The Windows bootstrap has no seed, so a Windows member with launch-fixed
// settings is refused rather than launched without them.
func TestDetachedWindowsMemberWithLaunchConfigIsRefused(t *testing.T) {
	boot := testDetachedBootstrap(t)
	boot.osOf = func(string, string) string { return "windows" }
	err := seedDetachedBootstrap(detachedObjectParams(), detachedObjectBase(), "sw-1", "sw", boot)
	if err == nil || !strings.Contains(err.Error(), "Windows member") {
		t.Fatalf("seedDetachedBootstrap = %v, want a refusal naming the Windows member", err)
	}
}
