package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// The production launcher must be the shared pkg/aws one: that is what gives a
// burst member the same RunInstances request and buildTags tag set as a
// `spawn launch` member (#905, #917).
var _ burstLauncher = (*spawnaws.Client)(nil)

// fakeBurstLauncher records each launch config burst asks for, without AWS.
type fakeBurstLauncher struct {
	got    []spawnaws.LaunchConfig
	failAt int // 1-based launch to fail; 0 = never
}

func (f *fakeBurstLauncher) Launch(_ context.Context, cfg spawnaws.LaunchConfig) (*spawnaws.LaunchResult, error) {
	f.got = append(f.got, cfg)
	if f.failAt == len(f.got) {
		return nil, errors.New("InsufficientInstanceCapacity")
	}
	return &spawnaws.LaunchResult{InstanceID: fmt.Sprintf("i-burst%d", len(f.got))}, nil
}

func burstTestParams() *LaunchParams {
	return &LaunchParams{
		Count:           3,
		InstanceType:    "t3.micro",
		AMI:             "ami-0123456789abcdef0",
		Region:          "us-east-1",
		JobArrayID:      "genomics",
		JobArrayName:    "genomics",
		InstanceProfile: "spored-instance-profile",
		TTL:             defaultBurstTTL,
	}
}

// launchedBurst drives the real launch function and returns what it asked for.
func launchedBurst(t *testing.T, params *LaunchParams) []spawnaws.LaunchConfig {
	t.Helper()
	fake := &fakeBurstLauncher{}
	ids, err := launchBurstInstances(context.Background(), fake, params)
	if err != nil {
		t.Fatalf("launchBurstInstances: %v", err)
	}
	if len(fake.got) != params.Count || len(ids) != params.Count {
		t.Fatalf("launched %d members (%d ids), want %d", len(fake.got), len(ids), params.Count)
	}
	return fake.got
}

// TestBurstLaunchAttachesTheInstanceProfile is #905: burst members were launched
// with no IamInstanceProfile, so spored on them had no credentials for its tags,
// the hybrid registry, DNS or self-termination.
func TestBurstLaunchAttachesTheInstanceProfile(t *testing.T) {
	for _, m := range launchedBurst(t, burstTestParams()) {
		if m.IamInstanceProfile != "spored-instance-profile" {
			t.Fatalf("member %d IamInstanceProfile = %q, want spored-instance-profile — "+
				"without it spored on a burst member has no AWS credentials (#905)", m.JobArrayIndex, m.IamInstanceProfile)
		}
	}
}

// TestBurstLaunchRefusesWithoutAnInstanceProfile: the guard lives where the
// member configs are built, so a future caller that forgets to resolve a profile
// cannot launch credential-less, unmanageable instances.
func TestBurstLaunchRefusesWithoutAnInstanceProfile(t *testing.T) {
	params := burstTestParams()
	params.InstanceProfile = ""
	fake := &fakeBurstLauncher{}
	if _, err := launchBurstInstances(context.Background(), fake, params); err == nil {
		t.Fatal("launchBurstInstances launched with no instance profile; want a refusal")
	}
	if len(fake.got) != 0 {
		t.Fatal("Launch was called despite the missing instance profile")
	}
}

// TestBurstMembersHaveATTL is #917: burst set no TTL, so nothing bounded a
// member's lifetime. Every member must carry the TTL buildTags turns into
// spawn:ttl and spawn:ttl-deadline, and the default must be a real, positive
// duration rather than "no TTL".
func TestBurstMembersHaveATTL(t *testing.T) {
	if err := validateBurstParams(&LaunchParams{Count: 1, TTL: defaultBurstTTL}); err != nil {
		t.Fatalf("defaultBurstTTL %q is not a valid burst TTL: %v", defaultBurstTTL, err)
	}
	if f := burstCmd.Flags().Lookup("ttl"); f == nil || f.DefValue != defaultBurstTTL {
		t.Fatalf("burst --ttl flag default = %v, want %q", f, defaultBurstTTL)
	}
	for _, m := range launchedBurst(t, burstTestParams()) {
		if m.TTL != defaultBurstTTL {
			t.Errorf("member %d TTL = %q, want %q", m.JobArrayIndex, m.TTL, defaultBurstTTL)
		}
	}
}

// TestBurstRejectsTTLsWithNoDeadline: buildTags computes spawn:ttl-deadline with
// time.ParseDuration, and the reaper enforces the deadline. A TTL it cannot parse
// would give a member spawn:ttl but no deadline, so it must be refused before
// anything launches; so must an empty or non-positive one.
func TestBurstRejectsTTLsWithNoDeadline(t *testing.T) {
	for _, ttl := range []string{"", "1d", "forever", "0s", "-1h"} {
		params := burstTestParams()
		params.TTL = ttl
		fake := &fakeBurstLauncher{}
		if _, err := launchBurstInstances(context.Background(), fake, params); err == nil {
			t.Errorf("--ttl %q accepted; want a refusal", ttl)
		}
		if len(fake.got) != 0 {
			t.Errorf("--ttl %q: Launch was called", ttl)
		}
	}
}

// TestBurstMembersAreDistinctJobArrayMembers is #917: one RunInstances call for
// all members gave them identical tags, so every member registered at index 0.
// Each member must carry its own index and the array's size and name, which is
// what buildTags writes as spawn:job-array-* and what `spawn array status`
// groups on.
func TestBurstMembersAreDistinctJobArrayMembers(t *testing.T) {
	params := burstTestParams()
	members := launchedBurst(t, params)
	for i, m := range members {
		if m.JobArrayIndex != i {
			t.Errorf("member %d has JobArrayIndex %d", i, m.JobArrayIndex)
		}
		if m.JobArraySize != params.Count || m.JobArrayID != params.JobArrayID || m.JobArrayName != params.JobArrayName {
			t.Errorf("member %d job array = (%q, %q, size %d), want (%q, %q, size %d)",
				i, m.JobArrayID, m.JobArrayName, m.JobArraySize, params.JobArrayID, params.JobArrayName, params.Count)
		}
		if m.OnComplete != "terminate" {
			t.Errorf("member %d OnComplete = %q, want terminate", i, m.OnComplete)
		}
	}
}

// TestBurstMembersAreTaggedBurst: spored joins the hybrid registry only for
// instances carrying spawn:burst=true (#904). buildTags never writes it (spawn
// launch members must not carry it), so burst must add it as its own tag on
// every member.
func TestBurstMembersAreTaggedBurst(t *testing.T) {
	for _, m := range launchedBurst(t, burstTestParams()) {
		if m.Tags[tagprefix.Tag("burst")] != "true" {
			t.Errorf("member %d tags %v lack %s=true", m.JobArrayIndex, m.Tags, tagprefix.Tag("burst"))
		}
	}
}

// TestBurstUserDataIsBase64: RunInstances rejects user-data that is not base64
// ("Invalid BASE64 encoding of user data", #127), and Client.Launch passes
// LaunchConfig.UserData through verbatim. Burst passed the raw script, so every
// burst failed at launch. The payload must decode (and gunzip, as cloud-init
// does) back to the burst script.
func TestBurstUserDataIsBase64(t *testing.T) {
	params := burstTestParams()
	for _, m := range launchedBurst(t, params) {
		raw, err := base64.StdEncoding.DecodeString(m.UserData)
		if err != nil {
			t.Fatalf("UserData is not base64 — RunInstances will reject it (#127): %v", err)
		}
		script := raw
		if zr, zerr := gzip.NewReader(bytes.NewReader(raw)); zerr == nil {
			if script, err = io.ReadAll(zr); err != nil {
				t.Fatalf("gunzip user-data: %v", err)
			}
		}
		if string(script) != generateBurstUserData(params) {
			t.Fatalf("decoded user-data is not the burst script:\n%s", script)
		}
	}
}

// TestBurstPartialFailureReturnsTheLaunchedIDs: members launch one at a time, so
// a failure part-way leaves earlier members running. Their IDs must come back
// with the error so the caller can report them rather than lose track of
// billable instances.
func TestBurstPartialFailureReturnsTheLaunchedIDs(t *testing.T) {
	fake := &fakeBurstLauncher{failAt: 3}
	ids, err := launchBurstInstances(context.Background(), fake, burstTestParams())
	if err == nil {
		t.Fatal("want an error from the failed third launch")
	}
	if strings.Join(ids, ",") != "i-burst1,i-burst2" {
		t.Fatalf("ids = %v, want the two members launched before the failure", ids)
	}
}

// TestResolveBurstInstanceProfile: --instance-profile is used verbatim with no IAM
// calls (as `spawn launch --instance-profile`, #550); otherwise the shared spored
// profile is set up, and a setup failure stops the burst before anything launches.
func TestResolveBurstInstanceProfile(t *testing.T) {
	ctx := context.Background()

	called := false
	setup := func(context.Context) (string, error) { called = true; return "spored-instance-profile", nil }

	got, err := resolveBurstInstanceProfile(ctx, "my-scoped-profile", setup)
	if err != nil || got != "my-scoped-profile" {
		t.Fatalf("explicit profile: got %q, %v; want my-scoped-profile", got, err)
	}
	if called {
		t.Fatal("explicit --instance-profile must bypass spored IAM setup entirely")
	}

	got, err = resolveBurstInstanceProfile(ctx, "", setup)
	if err != nil || got != "spored-instance-profile" || !called {
		t.Fatalf("default: got %q, %v (setup called=%v); want spored-instance-profile from setup", got, err, called)
	}

	boom := errors.New("AccessDenied: iam:CreateRole")
	if _, err := resolveBurstInstanceProfile(ctx, "", func(context.Context) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("setup failure: got %v, want it wrapped", err)
	}
	if _, err := resolveBurstInstanceProfile(ctx, "", func(context.Context) (string, error) { return "", nil }); err == nil {
		t.Fatal("setup returning an empty profile must be an error, not a credential-less launch")
	}
}
