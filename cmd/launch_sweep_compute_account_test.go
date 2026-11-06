package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/sweep"
)

// The keys of the fake profiles computeAccountSweep defines. teamDevKey is the
// profile that was compute's hardcoded default until #1538: defined here so a
// regression resolves it and signs with its key, rather than failing to load.
const (
	callerKey      = "AKIDCALLER"
	teamComputeKey = "AKIDTEAMCOMPUTE"
	teamDevKey     = "AKIDTEAMDEV"
)

// keyRecorder records the access key of every config it is handed.
type keyRecorder struct {
	mu   sync.Mutex
	keys []string
}

func (k *keyRecorder) add(t *testing.T, cfg awssdk.Config) {
	t.Helper()
	c, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Errorf("retrieve credentials: %v", err)
		return
	}
	k.mu.Lock()
	k.keys = append(k.keys, c.AccessKeyID)
	k.mu.Unlock()
}

func (k *keyRecorder) all() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keys...)
}

// computeAccountSweep runs a default (detached) sweep through runLaunch
// against the recording AWS fake, with SPAWN_COMPUTE_PROFILE unset, a fake
// AWS config holding the caller's profile, a team compute profile and the old
// hardcoded default, and setup applied to the environment. It returns stderr,
// the fake, the keys the role check and newComputeClient were handed, and the
// error.
func computeAccountSweep(t *testing.T, roleErr error, setup func(home string)) (out string, f *tagLimitAWS, roleKeys, clientKeys *keyRecorder, err error) {
	t.Helper()
	f = serveTagLimitAWS(t)
	withLogDir(t, false)
	t.Setenv("SPAWN_INFRA_PROFILE", "")
	t.Setenv("SPAWN_COMPUTE_PROFILE", "")
	_ = os.Unsetenv("SPAWN_COMPUTE_PROFILE")
	body := "[profile caller]\naws_access_key_id = " + callerKey + "\naws_secret_access_key = s\n" +
		"[profile teamcompute]\naws_access_key_id = " + teamComputeKey + "\naws_secret_access_key = s\n" +
		"[profile spore-host-" + "dev]\naws_access_key_id = " + teamDevKey + "\naws_secret_access_key = s\n"
	if werr := os.WriteFile(os.Getenv("AWS_CONFIG_FILE"), []byte(body), 0o600); werr != nil {
		t.Fatal(werr)
	}
	spawnconfig.ResetComputeNoticeForTest()
	stubTeamName(t)

	roleKeys, clientKeys = &keyRecorder{}, &keyRecorder{}
	role := &roleIAMFake{err: roleErr}
	prevRole := sweepRoleIAM
	t.Cleanup(func() { sweepRoleIAM = prevRole })
	sweepRoleIAM = func(cfg awssdk.Config) sweep.RoleGetter { roleKeys.add(t, cfg); return role }
	prevClient := newComputeClient
	t.Cleanup(func() { newComputeClient = prevClient })
	newComputeClient = func(ctx context.Context, region string) (*aws.Client, error) {
		c, cerr := prevClient(ctx, region)
		if cerr == nil {
			clientKeys.add(t, c.Config())
		}
		return c, cerr
	}

	setup(os.Getenv("HOME"))
	for k, v := range map[string]string{
		"param-file": dispatchSweepFile(t), "region": "us-east-1", "name": "acct", "yes": "true",
		"no-dns": "true", "ami": "ami-0123456789abcdef0", "os": "linux",
	} {
		setLaunchFlag(t, k, v)
	}
	out = captureStderr(t, func() { err = runLaunch(launchCmd, nil) })
	return out, f, roleKeys, clientKeys, err
}

func onlyKey(t *testing.T, what string, keys []string, want string) {
	t.Helper()
	if len(keys) == 0 {
		t.Errorf("%s: no credentials recorded", what)
	}
	for _, k := range keys {
		if k != want {
			t.Errorf("%s signed with %s, want only %s (%s is the old hardcoded compute default, #1538)", what, k, want, teamDevKey)
		}
	}
}

// With AWS_PROFILE alone, a detached sweep's role check is made in the
// caller's account, and the refusal creates nothing (#1538). The bug: the
// check, and the launch, used the spore-host-dev profile whatever
// AWS_PROFILE said.
func TestDetachedSweepRoleCheckIsInTheCallersAccount(t *testing.T) {
	out, f, roleKeys, _, err := computeAccountSweep(t, errNoCrossAccountRole, func(string) {
		t.Setenv("AWS_PROFILE", "caller")
	})
	if !isRoleRefusal(err) {
		t.Fatalf("err = %v, want the cross-account role refusal", err)
	}
	onlyKey(t, "the SpawnSweepCrossAccountRole check", roleKeys.all(), callerKey)
	signed := f.signingKeys()
	if signed[teamDevKey] != 0 || signed[callerKey] == 0 {
		t.Errorf("requests signed by key: %v, want the caller's only", signed)
	}
	if calls := f.beyondIdentity(); len(calls) != 0 {
		t.Errorf("the refused sweep made AWS call(s) beyond the identity lookup: %v", calls)
	}
	if want := "Members launch in account 123456789012 (profile caller, from AWS_PROFILE)"; !strings.Contains(out, want) {
		t.Errorf("stderr missing %q:\n%s", want, out)
	}
}

// compute_profile in ~/.spawn/config.yaml moves the compute account, for the
// role check and the line that names it.
func TestDetachedSweepRoleCheckFollowsComputeProfile(t *testing.T) {
	out, _, roleKeys, _, err := computeAccountSweep(t, errNoCrossAccountRole, func(home string) {
		t.Setenv("AWS_PROFILE", "caller")
		if mkErr := os.MkdirAll(filepath.Join(home, ".spawn"), 0o700); mkErr != nil {
			t.Fatal(mkErr)
		}
		if wErr := os.WriteFile(filepath.Join(home, ".spawn", "config.yaml"), []byte("defaults:\n  compute_profile: teamcompute\n"), 0o600); wErr != nil {
			t.Fatal(wErr)
		}
	})
	if !isRoleRefusal(err) {
		t.Fatalf("err = %v, want the cross-account role refusal", err)
	}
	onlyKey(t, "the SpawnSweepCrossAccountRole check", roleKeys.all(), teamComputeKey)
	if want := "(profile teamcompute, from compute_profile in ~/.spawn/config.yaml)"; !strings.Contains(out, want) {
		t.Errorf("stderr missing %q:\n%s", want, out)
	}
}

// Past the role check, what the detached sweep does for its members (the
// CLI IAM role, AMI and key seeding, subnet checks, instance profile, egress
// proxies) uses the same compute account, and nothing is signed with the old
// default (#1538). The fake refuses everything but the identity lookup, so
// the sweep stops early; every request it made before that is checked.
func TestDetachedSweepLaunchUsesTheComputeAccount(t *testing.T) {
	_, f, roleKeys, clientKeys, _ := computeAccountSweep(t, nil, func(string) {
		t.Setenv("AWS_PROFILE", "caller")
	})
	onlyKey(t, "the SpawnSweepCrossAccountRole check", roleKeys.all(), callerKey)
	onlyKey(t, "the detached path's compute clients", clientKeys.all(), callerKey)
	if len(clientKeys.all()) < 2 {
		t.Errorf("newComputeClient handed out %d client(s), want the IAM one and the member-region one", len(clientKeys.all()))
	}
	if len(f.beyondIdentity()) == 0 {
		t.Fatal("the sweep stopped before doing anything for its members; this test checks nothing")
	}
	if signed := f.signingKeys(); signed[teamDevKey] != 0 {
		t.Errorf("requests signed by key: %v; none may use %s", signed, teamDevKey)
	}
}

func TestMemberAccountLine(t *testing.T) {
	cp := spawnconfig.ComputeProfile{Name: "aws", Source: "AWS_PROFILE"}
	if got, want := memberAccountLine("942542972736", "", cp), "🏦 Members launch in account 942542972736 (profile aws, from AWS_PROFILE)"; got != want {
		t.Errorf("launch line = %q, want %q", got, want)
	}
	if got := memberAccountLine("942542972736", "942542972736", cp); !strings.Contains(got, "Members launch in account 942542972736 (profile aws") {
		t.Errorf("resume, same account: %q", got)
	}
	got := memberAccountLine("111111111111", "222222222222", cp)
	if !strings.Contains(got, "Members launch in account 222222222222 (the sweep's record)") || !strings.Contains(got, "account 111111111111 (profile aws, from AWS_PROFILE)") {
		t.Errorf("resume, other account: %q", got)
	}
}

// With compute_profile naming another account, what the members depend on is
// looked up and created there, not in the caller's account (#1538): the AMI
// lookup and the instance profile's IAM calls are signed by the compute
// profile. They used to be the caller's, so the members' key pair, instance
// profile and egress proxies could land in another account than the members.
func TestDetachedSweepMemberResourcesFollowComputeProfile(t *testing.T) {
	_, f, _, clientKeys, _ := computeAccountSweep(t, nil, func(home string) {
		t.Setenv("AWS_PROFILE", "caller")
		if mkErr := os.MkdirAll(filepath.Join(home, ".spawn"), 0o700); mkErr != nil {
			t.Fatal(mkErr)
		}
		if wErr := os.WriteFile(filepath.Join(home, ".spawn", "config.yaml"), []byte("defaults:\n  compute_profile: teamcompute\n"), 0o600); wErr != nil {
			t.Fatal(wErr)
		}
	})
	onlyKey(t, "the detached path's compute clients", clientKeys.all(), teamComputeKey)
	f.mu.Lock()
	defer f.mu.Unlock()
	checked := 0
	for i, a := range f.actions {
		switch a {
		case "DescribeImages", "GetRole", "CreateRole", "GetInstanceProfile", "CreateInstanceProfile":
			checked++
			if f.keys[i] != teamComputeKey {
				t.Errorf("%s signed with %s, want the compute profile's %s", a, f.keys[i], teamComputeKey)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no member-resource call reached the fake (%v); this test checks nothing", f.actions)
	}
}
