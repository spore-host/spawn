package config

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// isolateGuardEnv clears every source of an expected account and of ambient
// credentials, and gives the SDK a config file holding only static test
// profiles, so LoadDefaultConfig succeeds offline.
func isolateGuardEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"SPORE_ACCOUNT", "SPORE_PROFILE", "SPORE_REGION", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	} {
		t.Setenv(k, "")
	}
	cfgFile := filepath.Join(dir, "aws-config")
	body := "[default]\naws_access_key_id = AKIDDEFAULT\naws_secret_access_key = s\n" +
		"[profile spore-host-infra]\naws_access_key_id = AKIDINFRA\naws_secret_access_key = s\n" +
		"[profile spore-host-dev]\naws_access_key_id = AKIDDEV\naws_secret_access_key = s\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfgFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "aws-credentials"))
	t.Setenv("XDG_CONFIG_HOME", dir) // no spore config file
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	SetSharedFlags("", "", "")
	t.Cleanup(func() { SetSharedFlags("", "", "") })
}

// stubCaller makes CheckAccount see account for every config and counts calls.
func stubCaller(t *testing.T, account string, err error) *int {
	t.Helper()
	n := 0
	t.Cleanup(SetCallerAccountForTest(func(context.Context, aws.Config) (string, error) {
		n++
		return account, err
	}))
	return &n
}

func TestCheckAccount_NoExpectedAccountMakesNoCall(t *testing.T) {
	isolateGuardEnv(t)
	calls := stubCaller(t, "222222222222", nil)
	if err := CheckAccount(context.Background(), aws.Config{}); err != nil {
		t.Fatalf("CheckAccount with nothing pinned = %v, want nil", err)
	}
	if *calls != 0 {
		t.Fatalf("CheckAccount made %d STS lookups with no account pinned; want 0", *calls)
	}
}

// TestCheckAccount_RefusesAMismatch is the promise the docs make: when the user
// pins an account, spawn refuses to act if the credentials resolve to another
// one, from every source the shared config reads.
func TestCheckAccount_RefusesAMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		set          func(t *testing.T)
	}{
		{"flag", "--account", func(t *testing.T) { SetSharedFlags("", "", "111111111111") }},
		{"env", "SPORE_ACCOUNT", func(t *testing.T) { t.Setenv("SPORE_ACCOUNT", "111111111111") }},
		{"file", "spore config file", func(t *testing.T) {
			dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "spore")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[spore]\naccount = \"111111111111\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateGuardEnv(t)
			tc.set(t)
			stubCaller(t, "222222222222", nil)
			err := CheckAccount(context.Background(), aws.Config{})
			var mm *AccountMismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("CheckAccount = %v, want an AccountMismatchError", err)
			}
			msg := err.Error()
			for _, want := range []string{"111111111111", "222222222222", "refusing", tc.source} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not mention %q", msg, want)
				}
			}
		})
	}
}

func TestCheckAccount_MatchPasses(t *testing.T) {
	isolateGuardEnv(t)
	SetSharedFlags("", "", "111111111111")
	stubCaller(t, "111111111111", nil)
	if err := CheckAccount(context.Background(), aws.Config{}); err != nil {
		t.Fatalf("CheckAccount on a matching account = %v", err)
	}
}

// An unknown account is not a match: like truffle and lagotto, the guard fails
// closed when STS errors or names no account.
func TestCheckAccount_FailsClosed(t *testing.T) {
	for name, stub := range map[string]struct {
		account string
		err     error
	}{
		"sts error":     {"", errors.New("ExpiredToken")},
		"empty account": {"", nil},
	} {
		t.Run(name, func(t *testing.T) {
			isolateGuardEnv(t)
			SetSharedFlags("", "", "111111111111")
			stubCaller(t, stub.account, stub.err)
			err := CheckAccount(context.Background(), aws.Config{})
			if err == nil || !strings.Contains(err.Error(), "could not determine") {
				t.Fatalf("CheckAccount = %v, want a 'could not determine' refusal", err)
			}
		})
	}
}

// TestLoadComputeAWSConfig_EnforcesTheGuard: compute is where instances launch,
// so it is guarded whatever profile it resolves.
func TestLoadComputeAWSConfig_EnforcesTheGuard(t *testing.T) {
	isolateGuardEnv(t)
	SetSharedFlags("", "", "111111111111")
	calls := stubCaller(t, "222222222222", nil)
	cfg, err := LoadComputeAWSConfig(context.Background(), "us-east-1")
	var mm *AccountMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("LoadComputeAWSConfig = %v, want an AccountMismatchError", err)
	}
	if cfg.Credentials != nil {
		t.Error("LoadComputeAWSConfig handed out credentials alongside its refusal")
	}
	if *calls != 1 {
		t.Errorf("STS lookups = %d, want 1", *calls)
	}
}

// TestLoadInfraAWSConfig_GuardFollowsTheProfile: a dedicated infra profile is
// spawn's control-plane account, different by design, and is not guarded; infra
// resolved through the user's own profile is.
func TestLoadInfraAWSConfig_GuardFollowsTheProfile(t *testing.T) {
	t.Run("dedicated infra profile", func(t *testing.T) {
		isolateGuardEnv(t)
		SetSharedFlags("", "", "111111111111")
		calls := stubCaller(t, "222222222222", nil)
		if _, err := LoadInfraAWSConfig(context.Background(), "us-east-1"); err != nil {
			t.Fatalf("LoadInfraAWSConfig with the default spore-host-infra profile = %v, want nil", err)
		}
		if *calls != 0 {
			t.Errorf("STS lookups = %d, want 0 for a dedicated infra profile", *calls)
		}
	})
	t.Run("infra through the shared profile", func(t *testing.T) {
		isolateGuardEnv(t)
		t.Setenv("SPAWN_INFRA_PROFILE", "")
		SetSharedFlags("", "", "111111111111")
		stubCaller(t, "222222222222", nil)
		_, err := LoadInfraAWSConfig(context.Background(), "us-east-1")
		var mm *AccountMismatchError
		if !errors.As(err, &mm) {
			t.Fatalf("LoadInfraAWSConfig on the user's own credentials = %v, want an AccountMismatchError", err)
		}
	})
}

// stsResponder answers GetCallerIdentity for the real lookup, offline.
type stsResponder struct {
	account string
	calls   int
	hosts   []string
}

func (s *stsResponder) Do(req *http.Request) (*http.Response, error) {
	s.calls++
	s.hosts = append(s.hosts, req.URL.Host)
	body := `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::` +
		s.account + `:user/t</Arn><UserId>AIDA</UserId><Account>` + s.account +
		`</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`
	return &http.Response{StatusCode: 200, Request: req, Header: http.Header{"Content-Type": {"text/xml"}},
		Body: io.NopCloser(strings.NewReader(body))}, nil
}

// TestSTSCallerAccount_AsksSTSOncePerCredential exercises the real lookup (not
// the seam): it is GetCallerIdentity, works with no region configured, and is
// cached per access key so several clients cost one call.
func TestSTSCallerAccount_AsksSTSOncePerCredential(t *testing.T) {
	sts := &stsResponder{account: "333333333333"}
	cfg := aws.Config{
		HTTPClient:  sts,
		Credentials: credentials.NewStaticCredentialsProvider("AKIDCACHETEST", "s", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
	for i := 0; i < 3; i++ {
		got, err := stsCallerAccount(context.Background(), cfg)
		if err != nil || got != "333333333333" {
			t.Fatalf("stsCallerAccount = %q, %v", got, err)
		}
	}
	if sts.calls != 1 {
		t.Errorf("STS calls = %d, want 1 (cached per access key)", sts.calls)
	}
	if len(sts.hosts) == 0 || !strings.HasPrefix(sts.hosts[0], "sts.") {
		t.Errorf("lookup went to %v, want an STS endpoint", sts.hosts)
	}
}
