package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spf13/cobra"
)

// #1138: hermetic gates for spawn onboard's re-run, signing and output. IAM and
// the phone-home endpoint are recording fakes — nothing here reaches AWS or the
// real portal (the transport answers every request itself).

const (
	testPhoneHomeRole = "arn:aws:iam::111111111111:role/portal-phone-home"
	testOnboardRole   = "arn:aws:iam::222222222222:role/spore-portal-onboard"
	// A Function URL in us-east-1, the region the portal endpoint lives in.
	testPhoneHomeURL = "https://abcdefghijklmnop.lambda-url.us-east-1.on.aws/"
)

type fakeOnboardIAM struct {
	existing     *iamtypes.Role // nil → NoSuchEntity
	created      []*iam.CreateRoleInput
	trustUpdates []*iam.UpdateAssumeRolePolicyInput
	puts         []*iam.PutRolePolicyInput
}

func (f *fakeOnboardIAM) GetRole(_ context.Context, _ *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	if f.existing == nil {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("not found")}
	}
	return &iam.GetRoleOutput{Role: f.existing}, nil
}

func (f *fakeOnboardIAM) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.created = append(f.created, in)
	return &iam.CreateRoleOutput{Role: &iamtypes.Role{Arn: aws.String(testOnboardRole)}}, nil
}

func (f *fakeOnboardIAM) UpdateAssumeRolePolicy(_ context.Context, in *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	f.trustUpdates = append(f.trustUpdates, in)
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

func (f *fakeOnboardIAM) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.puts = append(f.puts, in)
	return &iam.PutRolePolicyOutput{}, nil
}

type recordingTransport struct{ reqs []*http.Request }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.reqs = append(r.reqs, req)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: http.Header{}}, nil
}

// existingRole is a role spawn created earlier, trusting externalID, in the
// URL-encoded form GetRole returns.
func existingRole(externalID string) *iamtypes.Role {
	return &iamtypes.Role{
		Arn:                      aws.String(testOnboardRole),
		AssumeRolePolicyDocument: aws.String(neturl.QueryEscape(portalTrustPolicy(testPhoneHomeRole, externalID))),
		Tags:                     []iamtypes.Tag{{Key: aws.String("Purpose"), Value: aws.String("spore-portal-onboard")}},
	}
}

type onboardRun struct {
	stdout, stderr bytes.Buffer
	iam            *fakeOnboardIAM
	http           *recordingTransport
	err            error
}

func runOnboardHermetic(t *testing.T, fake *fakeOnboardIAM, region, externalID, outputFormat string) *onboardRun {
	t.Helper()
	saved := struct {
		load                                    func(context.Context, string) (aws.Config, error)
		iamc                                    func(aws.Config) onboardIAMAPI
		caller                                  func(context.Context, aws.Config) (string, string, error)
		httpc                                   func() *http.Client
		region, url, role, spored, extID, outFm string
		skip, jsonOut                           bool
	}{loadAWSConfig, onboardIAMClient, onboardCaller, onboardHTTPClient,
		onboardRegion, onboardPhoneHomeURL, onboardPhoneHomeRole, onboardSporedRole, onboardExternalID, spawnOutputFormat,
		onboardSkipPhoneHome, onboardJSONOutput}
	t.Cleanup(func() {
		loadAWSConfig, onboardIAMClient, onboardCaller, onboardHTTPClient = saved.load, saved.iamc, saved.caller, saved.httpc
		onboardRegion, onboardPhoneHomeURL, onboardPhoneHomeRole = saved.region, saved.url, saved.role
		onboardSporedRole, onboardExternalID, spawnOutputFormat = saved.spored, saved.extID, saved.outFm
		onboardSkipPhoneHome, onboardJSONOutput = saved.skip, saved.jsonOut
	})

	r := &onboardRun{iam: fake, http: &recordingTransport{}}
	loadAWSConfig = func(context.Context, string) (aws.Config, error) {
		return aws.Config{Region: region, Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")}, nil
	}
	onboardIAMClient = func(aws.Config) onboardIAMAPI { return fake }
	onboardCaller = func(context.Context, aws.Config) (string, string, error) {
		return "222222222222", "arn:aws:iam::222222222222:user/tester", nil
	}
	onboardHTTPClient = func() *http.Client { return &http.Client{Transport: r.http} }
	onboardRegion, onboardPhoneHomeURL, onboardPhoneHomeRole = region, testPhoneHomeURL, testPhoneHomeRole
	onboardSporedRole, onboardExternalID, spawnOutputFormat = "", externalID, outputFormat
	onboardSkipPhoneHome, onboardJSONOutput = false, false

	c := &cobra.Command{}
	c.SetOut(&r.stdout)
	c.SetErr(&r.stderr)
	r.err = runOnboard(c, nil)
	return r
}

func (r *onboardRun) phoneHomeBody(t *testing.T) map[string]string {
	t.Helper()
	if len(r.http.reqs) != 1 {
		t.Fatalf("expected exactly one phone-home request, got %d", len(r.http.reqs))
	}
	b, err := io.ReadAll(r.http.reqs[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("phone-home body is not JSON: %v (%s)", err, b)
	}
	return m
}

// A re-run without --external-id must register the ExternalId the existing
// role trusts. Generating a fresh one sent the portal an ID the role rejects.
func TestOnboardRerunKeepsTheTrustedExternalID(t *testing.T) {
	fake := &fakeOnboardIAM{existing: existingRole("the-id-the-role-trusts")}
	r := runOnboardHermetic(t, fake, "us-east-1", "", "")
	if r.err != nil {
		t.Fatalf("re-run failed: %v\nstderr: %s", r.err, r.stderr.String())
	}
	if len(fake.created) != 0 {
		t.Fatalf("re-run created a role; it should have reused the existing one")
	}
	if got := r.phoneHomeBody(t)["externalId"]; got != "the-id-the-role-trusts" {
		t.Errorf("phone-home registered ExternalId %q; the role trusts %q, so the portal could never assume it", got, "the-id-the-role-trusts")
	}
	for _, u := range fake.trustUpdates {
		ids, err := trustedExternalIDs(aws.ToString(u.PolicyDocument))
		if err != nil || len(ids) != 1 || ids[0] != "the-id-the-role-trusts" {
			t.Errorf("re-run rewrote the trust policy to ExternalIds %v (err %v); it must keep the trusted one", ids, err)
		}
	}
	if !strings.Contains(r.stdout.String(), "the-id-the-role-trusts") {
		t.Errorf("the summary does not print the ExternalId the role trusts:\n%s", r.stdout.String())
	}
}

// When the trusted ExternalId cannot be determined, refuse rather than invent one.
func TestOnboardRerunRefusesWhenTheTrustedIDIsUnknown(t *testing.T) {
	role := existingRole("x")
	role.AssumeRolePolicyDocument = aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::111111111111:root"},"Action":"sts:AssumeRole"}]}`)
	fake := &fakeOnboardIAM{existing: role}
	r := runOnboardHermetic(t, fake, "us-east-1", "", "")
	if r.err == nil || !strings.Contains(r.err.Error(), "--external-id") {
		t.Fatalf("expected a refusal naming --external-id, got %v", r.err)
	}
	if len(r.http.reqs) != 0 || len(fake.trustUpdates) != 0 || len(fake.puts) != 0 {
		t.Errorf("refusal still wrote: %d phone-home, %d trust updates, %d policy puts", len(r.http.reqs), len(fake.trustUpdates), len(fake.puts))
	}
}

// A re-run must rewrite both inline policies to the current documents, so a
// role created before #817's scoping does not keep its old grants.
func TestOnboardRerunReconcilesInlinePolicies(t *testing.T) {
	fake := &fakeOnboardIAM{existing: existingRole("abc")}
	r := runOnboardHermetic(t, fake, "us-east-1", "", "")
	if r.err != nil {
		t.Fatalf("re-run failed: %v", r.err)
	}
	want := map[string]bool{"PortalEC2Launch": false, "PortalPassSporedProfile": false}
	for _, p := range fake.puts {
		if aws.ToString(p.RoleName) != portalOnboardRoleName {
			continue
		}
		if _, ok := want[aws.ToString(p.PolicyName)]; ok {
			want[aws.ToString(p.PolicyName)] = true
		}
		if aws.ToString(p.PolicyName) == "PortalEC2Launch" && !strings.Contains(aws.ToString(p.PolicyDocument), "ssm:resourceTag/spawn:managed") {
			t.Error("the re-put launch policy is not the current, #817-scoped document")
		}
	}
	for name, put := range want {
		if !put {
			t.Errorf("re-run did not rewrite inline policy %s; the role keeps whatever an older spawn granted", name)
		}
	}
}

// A role spawn did not create must not be rewritten.
func TestOnboardRefusesAForeignRoleOfTheSameName(t *testing.T) {
	role := existingRole("abc")
	role.Tags = nil
	fake := &fakeOnboardIAM{existing: role}
	r := runOnboardHermetic(t, fake, "us-east-1", "", "")
	if r.err == nil {
		t.Fatal("onboard rewrote a role it did not create")
	}
	if len(fake.trustUpdates)+len(fake.puts) != 0 {
		t.Errorf("refusal still wrote to the foreign role")
	}
}

// The phone-home is signed for the Function URL's region, not --region.
func TestOnboardSignsPhoneHomeForTheEndpointRegion(t *testing.T) {
	r := runOnboardHermetic(t, &fakeOnboardIAM{}, "eu-west-1", "", "")
	if r.err != nil {
		t.Fatalf("onboard failed: %v", r.err)
	}
	if len(r.http.reqs) != 1 {
		t.Fatalf("expected one phone-home request, got %d", len(r.http.reqs))
	}
	auth := r.http.reqs[0].Header.Get("Authorization")
	if !strings.Contains(auth, "/us-east-1/lambda/aws4_request") {
		t.Errorf("phone-home signature scope is not the endpoint's region (us-east-1): %q", auth)
	}
	if got := r.phoneHomeBody(t)["region"]; got != "eu-west-1" {
		t.Errorf("the onboarded region in the body is %q, want eu-west-1", got)
	}
}

// -o json must put exactly one JSON document on stdout and nothing else.
func TestOnboardJSONOutputIsOnlyJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *fakeOnboardIAM
	}{{"first run", &fakeOnboardIAM{}}, {"re-run", &fakeOnboardIAM{existing: existingRole("abc")}}} {
		t.Run(tc.name, func(t *testing.T) {
			r := runOnboardHermetic(t, tc.fake, "us-east-1", "", "json")
			if r.err != nil {
				t.Fatalf("onboard failed: %v", r.err)
			}
			dec := json.NewDecoder(bytes.NewReader(r.stdout.Bytes()))
			var m map[string]string
			if err := dec.Decode(&m); err != nil {
				t.Fatalf("stdout does not start with JSON: %v\n%s", err, r.stdout.String())
			}
			if rest, _ := io.ReadAll(dec.Buffered()); strings.TrimSpace(string(rest)) != "" || dec.More() {
				t.Errorf("stdout carries more than the JSON document:\n%s", r.stdout.String())
			}
			if m["roleArn"] != testOnboardRole || m["externalId"] == "" {
				t.Errorf("JSON result is missing fields: %v", m)
			}
			if !strings.Contains(r.stderr.String(), "Onboarding account") {
				t.Errorf("progress did not go to stderr: %q", r.stderr.String())
			}
		})
	}
}
