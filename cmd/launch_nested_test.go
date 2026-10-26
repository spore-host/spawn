package cmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
)

// #39: --nested-spawn is an opt-in role; the default launch and the default
// spored-instance-role are untouched; a CLI running AS the nested role makes no
// IAM writes.

func isolateNestedSpawn(t *testing.T) {
	t.Helper()
	isolateLaunchDNS(t)
	savedNested, savedPol, savedNoTimeout, savedTTL := nestedSpawn, iamPolicy, noTimeout, ttl
	savedARN, savedSetup := nestedCallerARN, setupNestedSpawnProfile
	nestedSpawn, iamPolicy, noTimeout, ttl = false, nil, false, ""
	t.Cleanup(func() {
		nestedSpawn, iamPolicy, noTimeout, ttl = savedNested, savedPol, savedNoTimeout, savedTTL
		nestedCallerARN, setupNestedSpawnProfile = savedARN, savedSetup
	})
}

func runEnsureIAMProfile(t *testing.T, c *aws.Client) (*aws.LaunchConfig, error) {
	t.Helper()
	cfg := &aws.LaunchConfig{}
	err := ensureIAMProfile(context.Background(), c, cfg, progress.NewQuietProgress(), audit.NewLogger(io.Discard, "", ""))
	return cfg, err
}

func TestNestedSpawnLaunchAttachesTheNestedRole(t *testing.T) {
	isolateNestedSpawn(t)
	nestedSpawn = true
	f := &fakeIAM{}
	cfg, err := runEnsureIAMProfile(t, newFakeIAMClient(t, f))
	if err != nil {
		t.Fatalf("ensureIAMProfile: %v", err)
	}
	want := aws.NestedSpawnRoleName()
	if cfg.IamInstanceProfile != want {
		t.Fatalf("--nested-spawn attached %q, want the nested role's profile %q", cfg.IamInstanceProfile, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	grant := f.roles[want]["spawn-scoped-policy"]
	for _, action := range []string{`"ec2:RunInstances"`, `"iam:PassRole"`, `"ec2:ImportKeyPair"`} {
		if !strings.Contains(grant, action) {
			t.Errorf("nested role %s carries no %s grant; policies: %v", want, action, policyNames(f.roles[want]))
		}
	}
	// The children's default identity must exist before the nested spore boots:
	// it cannot create it.
	if _, ok := f.roles[aws.SporedRoleName]; !ok {
		t.Errorf("--nested-spawn did not make sure %s exists for the children", aws.SporedRoleName)
	}
}

func TestDefaultLaunchIsUnchangedByNestedSpawn(t *testing.T) {
	isolateNestedSpawn(t)
	f := &fakeIAM{}
	cfg, err := runEnsureIAMProfile(t, newFakeIAMClient(t, f))
	if err != nil {
		t.Fatalf("ensureIAMProfile: %v", err)
	}
	if cfg.IamInstanceProfile != aws.SporedInstanceProfileName {
		t.Errorf("default launch attached %q, want %q", cfg.IamInstanceProfile, aws.SporedInstanceProfileName)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for role, policies := range f.roles {
		if aws.IsNestedSpawnRole(role) {
			t.Errorf("a default launch created the nested role %s", role)
		}
		for name, doc := range policies {
			for _, action := range []string{`"ec2:RunInstances"`, `"iam:PassRole"`} {
				if strings.Contains(doc, action) {
					t.Errorf("default launch wrote %s into %s/%s", action, role, name)
				}
			}
		}
	}
}

func TestLaunchFromANestedSporeWritesNoIAM(t *testing.T) {
	nested := aws.NestedSpawnRoleName()
	nestedCaller := func(context.Context, *aws.Client) (string, error) {
		return "arn:aws:sts::123456789012:assumed-role/" + nested + "/i-0abc", nil
	}
	cases := []struct {
		name        string
		wantNested  bool
		iamPolicies []string
		noTimeout   bool
		wantProfile string
		wantErr     string
	}{
		{name: "default child", wantProfile: aws.SporedInstanceProfileName},
		{name: "nested child", wantNested: true, wantProfile: nested},
		{name: "IAM flags refused", iamPolicies: []string{"s3:ReadOnly"}, wantErr: "cannot create IAM roles"},
		{name: "no-timeout refused", noTimeout: true, wantErr: "--no-timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateNestedSpawn(t)
			nestedCallerARN = nestedCaller
			setupCalls := 0
			setupNestedSpawnProfile = func(context.Context, *aws.Client) (string, error) {
				setupCalls++
				return "", nil
			}
			nestedSpawn, iamPolicy, noTimeout = tc.wantNested, tc.iamPolicies, tc.noTimeout
			f := &fakeIAM{}
			cfg, err := runEnsureIAMProfile(t, newFakeIAMClient(t, f))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("ensureIAMProfile: %v", err)
				}
				if cfg.IamInstanceProfile != tc.wantProfile {
					t.Errorf("profile = %q, want %q", cfg.IamInstanceProfile, tc.wantProfile)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.puts) != 0 || len(f.roles) != 0 || len(f.profiles) != 0 || setupCalls != 0 {
				t.Errorf("a launch from a nested spore wrote IAM: %d PutRolePolicy, roles %v, profiles %v, %d nested setups",
					len(f.puts), policyNamesOf(f.roles), f.profiles, setupCalls)
			}
		})
	}
}

func policyNamesOf(m map[string]map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNestedSpawnFlagValidation(t *testing.T) {
	cases := []struct {
		name    string
		set     func()
		wantErr string
	}{
		{"off is always fine", func() { noTimeout = true }, ""},
		{"alone is fine", func() { nestedSpawn = true }, ""},
		{"with --no-timeout", func() { nestedSpawn, noTimeout = true, true }, "--no-timeout"},
		{"with --instance-profile", func() { nestedSpawn, instanceProfile = true, "p" }, "--instance-profile"},
		{"with --iam-policy", func() { nestedSpawn, iamPolicy = true, []string{"s3:ReadOnly"} }, "--iam-role"},
		{"with a sweep", func() { nestedSpawn, params = true, "x" }, "sweep"},
		{"with a batch queue", func() { nestedSpawn, batchQueueFile = true, "q.json" }, "--batch-queue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateNestedSpawn(t)
			savedParams, savedQueue := params, batchQueueFile
			t.Cleanup(func() { params, batchQueueFile = savedParams, savedQueue })
			params, batchQueueFile = "", ""
			tc.set()
			err := validateNestedSpawnFlags()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// Cost control: an instance that can launch others must itself have a hard
// lifetime. An idle timeout alone is not one.
func TestNestedSpawnRequiresATTL(t *testing.T) {
	isolateNestedSpawn(t)
	nestedSpawn = true
	if err := requireNestedSpawnTTL(&aws.LaunchConfig{IdleTimeout: "1h"}); err == nil {
		t.Error("--nested-spawn with only an idle timeout was accepted")
	}
	if err := requireNestedSpawnTTL(&aws.LaunchConfig{TTL: "8h"}); err != nil {
		t.Errorf("--nested-spawn --ttl 8h refused: %v", err)
	}
	nestedSpawn = false
	if err := requireNestedSpawnTTL(&aws.LaunchConfig{}); err != nil {
		t.Errorf("a launch without --nested-spawn was held to the nested TTL rule: %v", err)
	}
}

// The checks must be reached by the real launch path, not only exist.
func TestLaunchCallsTheNestedSpawnChecks(t *testing.T) {
	src := readSourceFile(t, "launch_single.go")
	for _, call := range []string{
		"if err := validateNestedSpawnFlags(); err != nil",
		"if err := requireNestedSpawnTTL(config); err != nil",
		"if handled, err := ensureNestedSpawnProfile(ctx, awsClient, config); handled || err != nil",
	} {
		if !strings.Contains(src, call) {
			t.Errorf("launch_single.go does not call %q", call)
		}
	}
}
