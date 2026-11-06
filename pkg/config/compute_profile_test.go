package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// teamDevProfile is the profile that was compute's hardcoded default until
// #1538. The fake config below defines it, so a regression that falls back to
// it resolves real credentials and is caught by the access key, not only by
// the profile name.
const teamDevProfile = "spore-host-" + "dev"

// isolateComputeEnv is isolateGuardEnv with a fake AWS config defining one
// profile per precedence layer, each with its own access key, and an empty
// home (no ~/.spawn/config.yaml). It returns the home directory.
func isolateComputeEnv(t *testing.T) string {
	t.Helper()
	isolateGuardEnv(t)
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("[default]\naws_access_key_id = AKIDDEFAULT\naws_secret_access_key = s\n")
	for _, p := range []string{"envprof", "cfgprof", "awsprof", "flagprof", "sporeprof", "fileprof", teamDevProfile} {
		b.WriteString("[profile " + p + "]\naws_access_key_id = " + accessKeyFor(p) + "\naws_secret_access_key = s\n")
	}
	cfgFile := filepath.Join(dir, "aws-config")
	if err := os.WriteFile(cfgFile, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfgFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "aws-credentials"))
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	ResetComputeNoticeForTest()
	return dir
}

func accessKeyFor(profile string) string {
	return "AKID" + strings.ToUpper(strings.NewReplacer("-", "").Replace(profile))
}

func writeComputeSpawnConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".spawn"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".spawn", "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSporeConfig(t *testing.T, xdg, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(xdg, "spore"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "spore", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The precedence table (#1538): SPAWN_COMPUTE_PROFILE > compute_profile in
// ~/.spawn/config.yaml > the caller's own profile (--profile > SPORE_PROFILE
// > AWS_PROFILE > spore config file) > the ambient chain. No case resolves to
// the team's dev profile unless something names it.
func TestResolveComputeProfile_Precedence(t *testing.T) {
	unset := "<unset>"
	cases := []struct {
		name                        string
		env                         string // SPAWN_COMPUTE_PROFILE; unset = not in the environment
		configKey, flag, spore, aws string
		file                        string // profile in ~/.config/spore/config.toml
		want, wantSource, wantKey   string
		override                    bool
	}{
		{name: "env set beats everything", env: "envprof", configKey: "cfgprof", flag: "flagprof", aws: "awsprof",
			want: "envprof", wantSource: "SPAWN_COMPUTE_PROFILE", wantKey: "AKIDENVPROF", override: true},
		{name: "env empty skips the config key, uses the caller's", env: "", configKey: "cfgprof", aws: "awsprof",
			want: "awsprof", wantSource: "AWS_PROFILE", wantKey: "AKIDAWSPROF"},
		{name: "env empty, nothing else: ambient", env: "",
			want: "", wantSource: "", wantKey: "AKIDDEFAULT"},
		{name: "config key beats the caller's own", env: unset, configKey: "cfgprof", flag: "flagprof", aws: "awsprof",
			want: "cfgprof", wantSource: ComputeProfileConfigSource, wantKey: "AKIDCFGPROF", override: true},
		{name: "AWS_PROFILE only", env: unset, aws: "awsprof",
			want: "awsprof", wantSource: "AWS_PROFILE", wantKey: "AKIDAWSPROF"},
		{name: "--profile beats SPORE_PROFILE and AWS_PROFILE", env: unset, flag: "flagprof", spore: "sporeprof", aws: "awsprof",
			want: "flagprof", wantSource: "--profile", wantKey: "AKIDFLAGPROF"},
		{name: "SPORE_PROFILE beats AWS_PROFILE", env: unset, spore: "sporeprof", aws: "awsprof",
			want: "sporeprof", wantSource: "SPORE_PROFILE", wantKey: "AKIDSPOREPROF"},
		{name: "spore config file profile", env: unset, file: "fileprof",
			want: "fileprof", wantSource: "profile in the spore config file", wantKey: "AKIDFILEPROF"},
		{name: "nothing at all: ambient", env: unset,
			want: "", wantSource: "", wantKey: "AKIDDEFAULT"},
		{name: "asked for by name", env: unset, configKey: teamDevProfile, aws: "awsprof",
			want: teamDevProfile, wantSource: ComputeProfileConfigSource, wantKey: accessKeyFor(teamDevProfile), override: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateComputeEnv(t)
			if tc.env != unset {
				t.Setenv("SPAWN_COMPUTE_PROFILE", tc.env)
			}
			if tc.configKey != "" {
				writeComputeSpawnConfig(t, home, "defaults:\n  compute_profile: "+tc.configKey+"\n")
			}
			if tc.file != "" {
				writeSporeConfig(t, home, "[spore]\nprofile = \""+tc.file+"\"\n")
			}
			t.Setenv("SPORE_PROFILE", tc.spore)
			t.Setenv("AWS_PROFILE", tc.aws)
			SetSharedFlags(tc.flag, "", "")

			got := ResolveComputeProfile()
			if got.Name != tc.want || got.Source != tc.wantSource || got.Override != tc.override {
				t.Errorf("ResolveComputeProfile() = %+v, want {Name:%q Source:%q Override:%v}", got, tc.want, tc.wantSource, tc.override)
			}
			if GetComputeProfile() != tc.want {
				t.Errorf("GetComputeProfile() = %q, want %q", GetComputeProfile(), tc.want)
			}
			if got.Name == teamDevProfile && tc.want != teamDevProfile {
				t.Errorf("resolved %s, which nothing asked for (#1538)", teamDevProfile)
			}

			// And the config LoadComputeAWSConfig hands out carries that
			// profile's credentials.
			stubCaller(t, "111111111111", nil)
			cfg, err := LoadComputeAWSConfig(context.Background(), "us-east-1")
			if err != nil {
				t.Fatalf("LoadComputeAWSConfig: %v", err)
			}
			creds, err := cfg.Credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatalf("retrieve: %v", err)
			}
			if creds.AccessKeyID != tc.wantKey {
				t.Errorf("compute credentials = %s, want %s", creds.AccessKeyID, tc.wantKey)
			}
		})
	}
}

func TestComputeProfileDescribe(t *testing.T) {
	for _, tc := range []struct {
		p    ComputeProfile
		want string
	}{
		{ComputeProfile{Name: "aws", Source: "AWS_PROFILE"}, "profile aws, from AWS_PROFILE"},
		{ComputeProfile{Name: "x", Source: ComputeProfileConfigSource, Override: true}, "profile x, from compute_profile in ~/.spawn/config.yaml"},
		{ComputeProfile{}, "ambient credentials, no profile selected"},
	} {
		if got := tc.p.Describe(); got != tc.want {
			t.Errorf("%+v.Describe() = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// stubAccounts maps each config's access key, and the caller's own
// credentials, to an account, and counts the lookups.
func stubAccounts(t *testing.T, byKey map[string]string, ambient string) *int {
	t.Helper()
	n := 0
	t.Cleanup(SetCallerAccountForTest(func(ctx context.Context, cfg aws.Config) (string, error) {
		n++
		c, err := cfg.Credentials.Retrieve(ctx)
		if err != nil {
			return "", err
		}
		return byKey[c.AccessKeyID], nil
	}))
	t.Cleanup(SetAmbientAccountForTest(func(context.Context) (string, error) {
		n++
		return ambient, nil
	}))
	return &n
}

func captureNotice(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := ComputeNoticeOutput
	ComputeNoticeOutput = &buf
	t.Cleanup(func() { ComputeNoticeOutput = prev })
	return &buf
}

// The safety net (#1538): an override (env or config) that puts compute in
// another account than the caller's own prints a notice, once, naming both
// accounts and the override to unset. It does not refuse: split setups rely
// on it.
func TestComputeOverrideNotice(t *testing.T) {
	t.Run("config override in another account", func(t *testing.T) {
		home := isolateComputeEnv(t)
		writeComputeSpawnConfig(t, home, "defaults:\n  compute_profile: cfgprof\n")
		t.Setenv("AWS_PROFILE", "awsprof")
		stubAccounts(t, map[string]string{"AKIDCFGPROF": "435400000000"}, "942500000000")
		out := captureNotice(t)
		if _, err := LoadComputeAWSConfig(context.Background(), "us-east-1"); err != nil {
			t.Fatalf("an override in another account must not be refused: %v", err)
		}
		got := out.String()
		for _, want := range []string{"account 435400000000", "profile cfgprof, from compute_profile in ~/.spawn/config.yaml",
			"account 942500000000", "profile awsprof, from AWS_PROFILE", "Unset compute_profile in ~/.spawn/config.yaml"} {
			if !strings.Contains(got, want) {
				t.Errorf("notice missing %q:\n%s", want, got)
			}
		}
		out.Reset()
		if _, err := LoadComputeAWSConfig(context.Background(), "us-east-1"); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Errorf("notice printed twice:\n%s", out.String())
		}
	})
	t.Run("env override in another account", func(t *testing.T) {
		isolateComputeEnv(t)
		t.Setenv("SPAWN_COMPUTE_PROFILE", "envprof")
		stubAccounts(t, map[string]string{"AKIDENVPROF": "435400000000"}, "942500000000")
		out := captureNotice(t)
		if _, err := LoadComputeAWSConfig(context.Background(), "us-east-1"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "account 435400000000 (profile envprof, from SPAWN_COMPUTE_PROFILE)") ||
			!strings.Contains(out.String(), "ambient credentials, no profile selected") {
			t.Errorf("notice = %q", out.String())
		}
	})
	t.Run("override in the same account", func(t *testing.T) {
		isolateComputeEnv(t)
		t.Setenv("SPAWN_COMPUTE_PROFILE", "envprof")
		stubAccounts(t, map[string]string{"AKIDENVPROF": "942500000000"}, "942500000000")
		out := captureNotice(t)
		if _, err := LoadComputeAWSConfig(context.Background(), "us-east-1"); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Errorf("notice for the caller's own account: %q", out.String())
		}
	})
	t.Run("no override costs nothing", func(t *testing.T) {
		isolateComputeEnv(t)
		t.Setenv("AWS_PROFILE", "awsprof")
		calls := stubAccounts(t, nil, "942500000000")
		out := captureNotice(t)
		if _, err := LoadComputeAWSConfig(context.Background(), "us-east-1"); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || *calls != 0 {
			t.Errorf("no override: notice %q, %d account lookups; want none", out.String(), *calls)
		}
	})
	t.Run("override naming the caller's own profile", func(t *testing.T) {
		isolateComputeEnv(t)
		t.Setenv("AWS_PROFILE", "awsprof")
		t.Setenv("SPAWN_COMPUTE_PROFILE", "awsprof")
		calls := stubAccounts(t, nil, "942500000000")
		out := captureNotice(t)
		if _, err := LoadComputeAWSConfig(context.Background(), "us-east-1"); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || *calls != 0 {
			t.Errorf("notice %q, %d lookups; want none", out.String(), *calls)
		}
	})
}

// spawn defaults set compute-profile writes the key ResolveComputeProfile reads.
func TestComputeProfileDefaultKey(t *testing.T) {
	isolateComputeEnv(t)
	if err := SetLaunchDefault("compute-profile", "cfgprof"); err != nil {
		t.Fatal(err)
	}
	if got := ResolveComputeProfile(); got.Name != "cfgprof" || got.Source != ComputeProfileConfigSource {
		t.Errorf("after defaults set: %+v", got)
	}
	d, _ := LoadLaunchDefaults()
	if GetDefaultValue(d, "compute-profile") != "cfgprof" {
		t.Errorf("defaults list value = %q", GetDefaultValue(d, "compute-profile"))
	}
	if err := SetLaunchDefault("compute-profile", "has space"); err == nil {
		t.Error("a profile name with whitespace was accepted")
	}
	if err := UnsetLaunchDefault("compute-profile"); err != nil {
		t.Fatal(err)
	}
	if got := ResolveComputeProfile(); got.Name != "" || got.Override {
		t.Errorf("after unset: %+v, want the ambient chain", got)
	}
}
