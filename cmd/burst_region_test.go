package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// awsCall is one request a command sent, as the recorder saw it.
type awsCall struct {
	host   string
	action string // EC2 Query "Action" or the DynamoDB X-Amz-Target operation
}

// awsRecorder is an HTTP client that never reaches the network. It records
// every request. It answers DynamoDB DescribeTable with an ACTIVE table and STS
// GetCallerIdentity with a test account, so burst gets past EnsureTable and the
// launch's account lookup to RunInstances, and everything else, RunInstances
// included, with a non-retryable 403. Nothing is created and nothing launched.
type awsRecorder struct {
	mu    sync.Mutex
	calls []awsCall
}

func (r *awsRecorder) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	call := awsCall{host: req.URL.Host}
	if target := req.Header.Get("X-Amz-Target"); target != "" {
		call.action = target[strings.LastIndex(target, ".")+1:]
	} else if form, err := url.ParseQuery(string(body)); err == nil {
		call.action = form.Get("Action")
	}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()

	resp := &http.Response{Request: req, Header: http.Header{}}
	if call.action == "DescribeTable" {
		resp.StatusCode = http.StatusOK
		resp.Header.Set("Content-Type", "application/x-amz-json-1.0")
		resp.Body = io.NopCloser(strings.NewReader(`{"Table":{"TableName":"spawn-hybrid-registry","TableStatus":"ACTIVE"}}`))
		return resp, nil
	}
	if call.action == "GetCallerIdentity" {
		resp.StatusCode = http.StatusOK
		resp.Header.Set("Content-Type", "text/xml")
		resp.Body = io.NopCloser(strings.NewReader(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDATEST</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>test</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
		return resp, nil
	}
	resp.StatusCode = http.StatusForbidden
	resp.Header.Set("Content-Type", "text/xml")
	resp.Body = io.NopCloser(bytes.NewReader([]byte(
		`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>recorder: no AWS in tests</Message></Error></Errors><RequestID>test</RequestID></Response>`)))
	return resp, nil
}

func (r *awsRecorder) snapshot() []awsCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]awsCall(nil), r.calls...)
}

// recordAWS routes every AWS config package cmd loads through a recorder. The
// real loader runs first, so region and profile resolution are the production
// ones; only the transport and the credentials are replaced.
func recordAWS(t *testing.T) *awsRecorder {
	t.Helper()
	rec := &awsRecorder{}
	real := loadAWSConfig
	loadAWSConfig = func(ctx context.Context, region string) (sdkaws.Config, error) {
		cfg, err := real(ctx, region)
		if err != nil {
			return cfg, err
		}
		cfg.HTTPClient = rec
		// The hermetic wrapper (#1003) points AWS_ENDPOINT_URL at its loopback
		// recorder, which would hide the regional host these tests assert on.
		// This config never leaves the process (rec answers every request), so
		// let the SDK resolve the region's real endpoint name.
		cfg.BaseEndpoint = nil
		cfg.Credentials = credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRET", "")
		cfg.Retryer = func() sdkaws.Retryer { return sdkaws.NopRetryer{} }
		return cfg, nil
	}
	t.Cleanup(func() { loadAWSConfig = real })
	return rec
}

// isolateAWSEnv clears every ambient region and profile source and points the
// SDK and the shared spore config at empty files, so only what a test sets is
// in play.
func isolateAWSEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"SPORE_REGION", "SPORE_PROFILE", "SPORE_ACCOUNT",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "aws-credentials"))
	t.Setenv("XDG_CONFIG_HOME", dir) // no ~/.config/spore/config.toml
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	return dir
}

// runRootForTest executes the real root command, so the root's
// PersistentPreRunE records --region/--profile exactly as in production, and
// resets the global flag state afterwards.
func runRootForTest(t *testing.T, args ...string) error {
	t.Helper()
	t.Cleanup(func() {
		spawnRegion, spawnProfile, spawnAccount = "", "", ""
		spawnconfig.SetSharedFlags("", "", "")
		for _, name := range []string{"region", "profile", "account"} {
			if f := rootCmd.PersistentFlags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs(args)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	return rootCmd.ExecuteContext(context.Background())
}

// burstArgs is a burst that needs no IAM and no AMI lookup beyond the Windows
// check: --instance-profile and --ami are given.
func burstArgs(extra ...string) []string {
	return append([]string{
		"burst", "--count", "1", "--job-array-id", "region-test",
		"--instance-profile", "existing-profile", "--ami", "ami-0123456789abcdef0",
	}, extra...)
}

// assertBurstRegion checks that burst made its table check and its launch
// attempt, and that every request it sent to a regional service went to want.
func assertBurstRegion(t *testing.T, rec *awsRecorder, want string) {
	t.Helper()
	calls := rec.snapshot()
	var sawTable, sawLaunch bool
	for _, c := range calls {
		// The Price List and Account APIs have only a us-east-1 endpoint, which
		// the client pins on purpose; every regional service must go to want.
		if strings.HasPrefix(c.host, "api.pricing.") || strings.HasPrefix(c.host, "account.") {
			continue
		}
		if !strings.Contains(c.host, "."+want+".") {
			t.Errorf("burst sent %s to %s, want region %s", c.action, c.host, want)
		}
		switch {
		case c.action == "DescribeTable" && strings.HasPrefix(c.host, "dynamodb."):
			sawTable = true
		case c.action == "RunInstances" && strings.HasPrefix(c.host, "ec2."):
			sawLaunch = true
		}
	}
	if !sawTable {
		t.Errorf("burst never checked the registry table; calls: %+v", calls)
	}
	if !sawLaunch {
		t.Errorf("burst never reached RunInstances; calls: %+v", calls)
	}
}

// TestBurstHonoursTheRegionFlag is #939: `spawn burst --region us-west-2`
// launched in the ambient region. The registry table check and the
// RunInstances call must both go to --region, even when AWS_REGION disagrees,
// as the root flag's help promises ("overrides SPORE_REGION/AWS_REGION and the
// shared config").
func TestBurstHonoursTheRegionFlag(t *testing.T) {
	isolateAWSEnv(t)
	t.Setenv("AWS_REGION", "eu-west-1")
	rec := recordAWS(t)

	if err := runRootForTest(t, burstArgs("--region", "us-west-2")...); err == nil {
		t.Fatal("burst succeeded against a recorder that refuses RunInstances")
	}
	assertBurstRegion(t, rec, "us-west-2")
}

// TestBurstHonoursSporeRegion: with no --region, SPORE_REGION beats AWS_REGION,
// the shared config's documented precedence.
func TestBurstHonoursSporeRegion(t *testing.T) {
	isolateAWSEnv(t)
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("SPORE_REGION", "ap-northeast-1")
	rec := recordAWS(t)

	_ = runRootForTest(t, burstArgs()...)
	assertBurstRegion(t, rec, "ap-northeast-1")
}

// TestBurstHonoursTheProfileFlag: --profile selects the named AWS profile, and
// with no region given anywhere else, that profile's region is where burst
// launches.
func TestBurstHonoursTheProfileFlag(t *testing.T) {
	dir := isolateAWSEnv(t)
	cfgFile := filepath.Join(dir, "aws-config")
	if err := os.WriteFile(cfgFile, []byte("[default]\nregion = eu-west-1\n\n[profile burst-test]\nregion = ca-central-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := recordAWS(t)

	_ = runRootForTest(t, burstArgs("--profile", "burst-test")...)
	assertBurstRegion(t, rec, "ca-central-1")
}

// TestBurstRefusesToRunWithNoRegion: with no region anywhere, burst stops
// before any AWS call instead of sending requests the SDK cannot route.
func TestBurstRefusesToRunWithNoRegion(t *testing.T) {
	isolateAWSEnv(t)
	rec := recordAWS(t)

	err := runRootForTest(t, burstArgs()...)
	if err == nil || !strings.Contains(err.Error(), "no AWS region") {
		t.Fatalf("burst with no region = %v, want a no-region error", err)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("burst made %d AWS calls with no region: %+v", len(calls), calls)
	}
}
