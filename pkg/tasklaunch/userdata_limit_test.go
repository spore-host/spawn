package tasklaunch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// countingAWSTransport counts every HTTP request an AWS client sends, answers
// STS GetCallerIdentity and fails everything else, so a test can tell whether
// any AWS call was made at all.
type countingAWSTransport struct{ n atomic.Int32 }

func (c *countingAWSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	if strings.HasPrefix(req.URL.Host, "sts.") {
		// Answer GetCallerIdentity, so GetAccountID does not fall back to IMDS
		// (a network call the hermetic test run must not make).
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(
			`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
				`<Arn>arn:aws:iam::123456789012:user/t</Arn><UserId>AIDA</UserId><Account>123456789012</Account>` +
				`</GetCallerIdentityResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))}, nil
	}
	return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

func countingAWSClient() (*aws.Client, *countingAWSTransport) {
	tr := &countingAWSTransport{}
	return aws.NewClientFromConfig(sdkaws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
		Retryer:     func() sdkaws.Retryer { return retry.AddWithMaxAttempts(retry.NewStandard(), 1) },
	}), tr
}

// userDataSpec is a task whose inline command carries n characters of
// hex from a SHA-256 chain, which gzip cannot shrink much, so the user data grows with
// n the way a long inline script makes it grow.
func userDataSpec(n int) *taskproto.TaskSpec {
	var b strings.Builder
	sum := sha256.Sum256([]byte("spawn#1063"))
	for b.Len() < n {
		b.WriteString(hex.EncodeToString(sum[:]))
		sum = sha256.Sum256(sum[:])
	}
	return &taskproto.TaskSpec{
		TaskID:    "userdata-limit",
		Command:   []string{"bash", "-c", "echo " + b.String()[:n]},
		Resources: taskproto.ResourceRequest{CPU: 2, MemoryGiB: 4},
		Lifecycle: taskproto.Lifecycle{TTL: "1h", OnComplete: "terminate"},
	}
}

var userDataTestSized = &taskproto.SizeResult{InstanceType: "m7i.large", Family: "m7i", VCPUs: 2, MemoryGiB: 8, OnDemandPrice: 0.1}

func userDataTestFinder() fakeTaskFinder {
	return fakeTaskFinder{cands: []taskproto.Candidate{{InstanceType: "m7i.large", Family: "m7i", VCPUs: 2, MemoryGiB: 8, OnDemandPrice: 0.1}}}
}

// measuredUserData is the size EC2 checks, computed here independently of the
// code under test: the task's launch assembly, launcher.BuildUserData (the
// bytes Provision sends), then base64 decoding.
func measuredUserData(t *testing.T, spec *taskproto.TaskSpec) int {
	t.Helper()
	cfg, opts, err := buildTaskLaunch(spec, userDataTestSized, "us-east-1", preflightProfile,
		taskproto.EffectiveResultsPrefix(spec, preflightAccountID, "us-east-1"), preflightRunID, "")
	if err != nil {
		t.Fatalf("buildTaskLaunch: %v", err)
	}
	enc, err := launcher.BuildUserData(cfg, opts)
	if err != nil {
		t.Fatalf("BuildUserData: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("user data is not base64: %v", err)
	}
	return len(raw)
}

// boundarySpecs returns the largest spec whose user data fits EC2's 16,384-byte
// limit and the smallest one that does not, found by measuring.
func boundarySpecs(t *testing.T) (under, over *taskproto.TaskSpec, underBytes, overBytes int) {
	t.Helper()
	lo, hi := 0, 64*1024
	if measuredUserData(t, userDataSpec(lo)) > aws.MaxUserDataBytes {
		t.Fatal("an empty command is already over the limit")
	}
	if measuredUserData(t, userDataSpec(hi)) <= aws.MaxUserDataBytes {
		t.Fatal("64 KiB of hex still fits; the fixture does not reach the limit")
	}
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if measuredUserData(t, userDataSpec(mid)) <= aws.MaxUserDataBytes {
			lo = mid
		} else {
			hi = mid
		}
	}
	under, over = userDataSpec(lo), userDataSpec(hi)
	return under, over, measuredUserData(t, under), measuredUserData(t, over)
}

// TestTaskRunRefusesUserDataOverTheEC2Limit is the spawn#1063 gate. A spec
// whose generated user data is over EC2's 16 KiB limit passed --dry-run and
// then failed at RunInstances with a 400. Now both the dry run and the real
// path refuse it before any AWS call (counted by a transport that sees every
// request), and a spec just under the limit passes both.
func TestTaskRunRefusesUserDataOverTheEC2Limit(t *testing.T) {
	under, over, underBytes, overBytes := boundarySpecs(t)
	if underBytes > aws.MaxUserDataBytes || overBytes <= aws.MaxUserDataBytes {
		t.Fatalf("boundary search broke: under=%d over=%d", underBytes, overBytes)
	}
	t.Logf("boundary: %d-byte command -> %d bytes, next -> %d bytes", len(under.Command[2]), underBytes, overBytes)

	t.Run("dry run refuses the oversized spec", func(t *testing.T) {
		var out bytes.Buffer
		err := renderDryRun(context.Background(), &out, over, userDataTestFinder(), "us-east-1")
		if err == nil {
			t.Fatalf("dry run accepted a spec whose user data is %d bytes\n%s", overBytes, out.String())
		}
		for _, want := range []string{fmt.Sprint(overBytes), "16384", "inputs:"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q: %v", want, err)
			}
		}
	})

	t.Run("dry run passes and reports the spec just under", func(t *testing.T) {
		var out bytes.Buffer
		if err := renderDryRun(context.Background(), &out, under, userDataTestFinder(), "us-east-1"); err != nil {
			t.Fatalf("dry run refused a spec whose user data is %d bytes: %v", underBytes, err)
		}
		want := fmt.Sprintf("User data:    %d of 16384 bytes", underBytes)
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run does not report %q\n%s", want, out.String())
		}
	})

	t.Run("real run refuses the oversized spec before any AWS call", func(t *testing.T) {
		client, tr := countingAWSClient()
		err := runTaskReal(context.Background(), io.Discard, client, over, userDataTestFinder(), "us-east-1")
		if err == nil || !strings.Contains(err.Error(), "16384") {
			t.Fatalf("runTaskReal = %v, want a user-data size refusal", err)
		}
		if n := tr.n.Load(); n != 0 {
			t.Errorf("runTaskReal made %d AWS request(s) before refusing an oversized spec; want 0", n)
		}
	})

	t.Run("real run lets the spec just under through", func(t *testing.T) {
		client, tr := countingAWSClient()
		err := runTaskReal(context.Background(), io.Discard, client, under, userDataTestFinder(), "us-east-1")
		// The counting transport fails every request after GetCallerIdentity,
		// so the run must stop at an AWS call: proof it got past the size check.
		if err == nil {
			t.Fatal("runTaskReal succeeded against a transport that fails every request")
		}
		var tooLarge *aws.UserDataTooLargeError
		if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "16384") {
			t.Fatalf("runTaskReal refused a spec whose user data is %d bytes: %v", underBytes, err)
		}
		if tr.n.Load() == 0 {
			t.Errorf("runTaskReal made no AWS request for a spec under the limit; it stopped somewhere else: %v", err)
		}
	})
}

// TestTaskUserDataPreflightMatchesTheLaunch: the preflight measures the same
// bytes the real launch sends. With a results_prefix given (so no placeholder
// account is involved) and the run id the preflight uses, the two are equal.
func TestTaskUserDataPreflightMatchesTheLaunch(t *testing.T) {
	// Any size under the limit will do; this compares two measurements, it does
	// not probe the limit (the bisection above does). 2000 stopped fitting when
	// the wrapper's IMDS lookup (spawn#1260) added ~200 bytes.
	spec := userDataSpec(1500)
	spec.ResultsPrefix = "s3://my-bucket/tasks/x"
	got, _, err := taskUserDataPreflight(spec, userDataTestSized, "us-east-1", false)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	prefix := taskproto.EffectiveResultsPrefix(spec, "123456789012", "us-east-1")
	cfg, opts, err := buildTaskLaunch(spec, userDataTestSized, "us-east-1", "a-real-profile", prefix, preflightRunID, "")
	if err != nil {
		t.Fatalf("buildTaskLaunch: %v", err)
	}
	enc, err := launcher.BuildUserData(cfg, opts)
	if err != nil {
		t.Fatalf("BuildUserData: %v", err)
	}
	if want, _ := aws.UserDataSize(enc); got != want {
		t.Errorf("preflight measured %d bytes, the launch would send %d", got, want)
	}
}

// runTaskReal launches spec through Launch with client's real dependencies and
// finder, as `spawn task run` does.
func runTaskReal(ctx context.Context, out io.Writer, client *aws.Client, spec *taskproto.TaskSpec, finder taskproto.InstanceFinder, region string) error {
	_, err := Launch(ctx, Deps{Finder: finder, AWS: client, Provisioner: LauncherProvisioner(client), Out: out}, spec, region)
	return err
}
