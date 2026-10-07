package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mockProber lets each probe be stubbed per-test. Nil error funcs default to pass.
type mockProber struct {
	expectedAccount string
	credErr         error
	acct            string
	regionErr       error
	ec2DescribeErr  error
	ec2LaunchErr    error
	iamErr          error
	sporedErr       error
	vpcErr          error
	ssmErr          error
	reaperErr       error
	nitroErr        error
	route53Err      error
	truffleErr      error
	awscliErr       error
	sshErr          error
}

func (m mockProber) SpawnVersion() string                           { return "1.2.3" }
func (m mockProber) TruffleAvailable() (string, error)              { return "1.0.0", m.truffleErr }
func (m mockProber) AWSCLIAvailable() (string, error)               { return "aws-cli/2.32.0", m.awscliErr }
func (m mockProber) SSHKeyPresent() (string, error)                 { return "~/.ssh/id_ed25519", m.sshErr }
func (m mockProber) Credentials(context.Context) (string, error)    { return m.acct, m.credErr }
func (m mockProber) ExpectedAccount() string                        { return m.expectedAccount }
func (m mockProber) Region(context.Context) (string, error)         { return "us-east-1", m.regionErr }
func (m mockProber) EC2Describe(context.Context) error              { return m.ec2DescribeErr }
func (m mockProber) EC2LaunchPermission(context.Context) error      { return m.ec2LaunchErr }
func (m mockProber) IAMInstanceProfileAccess(context.Context) error { return m.iamErr }
func (m mockProber) SporedRolePresent(context.Context) (string, error) {
	return "spored-instance-role", m.sporedErr
}
func (m mockProber) VPCAndSubnet(context.Context) (string, error) {
	return "vpc-abc / 3 subnets", m.vpcErr
}
func (m mockProber) SSMAvailable(context.Context) (string, error)     { return "reachable", m.ssmErr }
func (m mockProber) ReaperConfigured(context.Context) (string, error) { return "enforce", m.reaperErr }
func (m mockProber) Route53Available(context.Context) (string, error) {
	return "12 zones", m.route53Err
}
func (m mockProber) NitroCoverage(context.Context) (string, error) {
	return "163 Nitro instance types (v6:44 v5:14 v4:53 v3:19 v2:33)", m.nitroErr
}

func find(r *Report, name string) *Check {
	for i := range r.Checks {
		if r.Checks[i].Name == name {
			return &r.Checks[i]
		}
	}
	return nil
}

func TestRun_AllPass(t *testing.T) {
	r := Run(context.Background(), mockProber{acct: "123456789012"})
	if !r.OK() {
		t.Fatalf("expected OK, got not-OK; checks: %+v", r.Checks)
	}
	// account shows the resolved id
	if c := find(r, "account"); c == nil || c.Status != Pass || c.Detail != "123456789012" {
		t.Errorf("account check = %+v", c)
	}
}

func TestRun_BadCredentialsSkipsDownstream(t *testing.T) {
	r := Run(context.Background(), mockProber{credErr: errors.New("no creds")})
	if r.OK() {
		t.Fatal("expected not-OK when credentials fail")
	}
	if c := find(r, "AWS credentials"); c == nil || c.Status != Fail {
		t.Errorf("credentials should Fail, got %+v", c)
	}
	// Downstream AWS checks must be Skip, not Fail (no cascade).
	for _, name := range []string{"region", "EC2 describe permission", "VPC & subnet"} {
		if c := find(r, name); c == nil || c.Status != Skip {
			t.Errorf("%s should be Skip after cred failure, got %+v", name, c)
		}
	}
}

func TestRun_ExpectedAccountMismatchFails(t *testing.T) {
	r := Run(context.Background(), mockProber{acct: "111111111111", expectedAccount: "999999999999"})
	c := find(r, "account")
	if c == nil || c.Status != Fail {
		t.Fatalf("account mismatch should Fail, got %+v", c)
	}
	if !strings.Contains(c.Fix, "999999999999") {
		t.Errorf("fix should mention the expected account, got %q", c.Fix)
	}
	if r.OK() {
		t.Error("report should not be OK on account mismatch")
	}
}

func TestRun_CorePermFailIsFatal_OptionalIsWarn(t *testing.T) {
	r := Run(context.Background(), mockProber{
		acct:         "123456789012",
		ec2LaunchErr: errors.New("UnauthorizedOperation"),
		reaperErr:    errors.New("not configured"),
		route53Err:   errors.New("no zones"),
	})
	if c := find(r, "EC2 launch permission"); c == nil || c.Status != Fail {
		t.Errorf("EC2 launch perm should Fail, got %+v", c)
	}
	if c := find(r, "TTL reaper backstop"); c == nil || c.Status != Warn {
		t.Errorf("reaper should Warn (optional), got %+v", c)
	}
	if c := find(r, "Route 53 (DNS)"); c == nil || c.Status != Warn {
		t.Errorf("route53 should Warn (optional), got %+v", c)
	}
	if r.OK() {
		t.Error("a core permission Fail must make the report not-OK")
	}
}

func TestRun_WarnsDoNotBlock(t *testing.T) {
	// Only optional/warn-level probes fail → report is still OK (launchable).
	r := Run(context.Background(), mockProber{
		acct:       "123456789012",
		truffleErr: errors.New("not found"),
		reaperErr:  errors.New("none"),
		sshErr:     errors.New("no key"),
	})
	if !r.OK() {
		t.Errorf("warnings alone should not block; checks: %+v", r.Checks)
	}
}

// TestReaperWarnDoesNotPrependAnUnprovenClaim is the doctor half of spawn#624.
//
// The reaper line used to be assembled here as
//
//	"no out-of-band reaper configured; TTL is enforced in-instance by spored only: " + errText(err)
//
// prepended to EVERY non-nil result — including the case where the probe could not
// determine coverage at all. Combined with a prober that always returned an error,
// the check printed an identical warning in every account whether a reaper covered it
// or not, which is why a real 13-day leak went unremarked. The consequence is now
// stated by the prober, which knows which case it is.
func TestReaperWarnDoesNotPrependAnUnprovenClaim(t *testing.T) {
	r := Run(context.Background(), mockProber{
		reaperErr: errors.New("could not determine coverage (iam:GetRole: AccessDenied)"),
	})

	c := find(r, "TTL reaper backstop")
	if c == nil {
		t.Fatal("no reaper check in the report")
	}
	if c.Status != Warn {
		t.Errorf("status = %v, want Warn", c.Status)
	}
	if strings.Contains(c.Fix, "no out-of-band reaper configured") {
		t.Errorf("doctor must not assert the reaper is absent when the probe only said it "+
			"could not tell; the prober's own wording should pass through:\n%s", c.Fix)
	}
	if !strings.Contains(c.Fix, "could not determine coverage") {
		t.Errorf("the prober's verdict must survive into the finding:\n%s", c.Fix)
	}
}

// TestReaperPassNamesItsEvidence: a Pass has to say HOW coverage was established, so
// an operator can tell an in-account reaper from a cross-account grant.
func TestReaperPassNamesItsEvidence(t *testing.T) {
	r := Run(context.Background(), mockProber{}) // reaperErr nil => Pass with detail "enforce"
	c := find(r, "TTL reaper backstop")
	if c == nil || c.Status != Pass {
		t.Fatalf("expected a Pass, got %+v", c)
	}
	if c.Detail == "" {
		t.Error("a passing reaper check must report the evidence it found, not just pass silently")
	}
}

// TestNitroCoverageIsAWarnNotAFail: every region has some Nitro capacity, so
// this check can never block a launch. It reports an environment constraint —
// the same category as Route 53 access or reaper coverage — and must not turn a
// ready account into a failing one.
func TestNitroCoverageIsAWarnNotAFail(t *testing.T) {
	r := Run(context.Background(), mockProber{nitroErr: errors.New("newest generation is v3")})

	c := find(r, "Nitro fleet")
	if c == nil {
		t.Fatal("no 'Nitro fleet' check in the report")
	}
	if c.Status != Warn {
		t.Errorf("status = %v, want Warn — a region's Nitro fleet is a constraint, not a "+
			"prerequisite, and cannot fail a launch", c.Status)
	}
	// And it must not drag the whole report down: doctor exits non-zero only on
	// Fail, so a warn here has to leave OK() true.
	if !r.OK() {
		t.Error("a Nitro warning made the report not-OK, so 'spawn doctor' would exit 1 " +
			"on an account that is perfectly able to launch")
	}
}

// TestNitroCoveragePassReportsTheSpread: the detail is the whole value of the
// check, since it cannot fail. A pass with an empty detail would be a line that
// says nothing.
func TestNitroCoveragePassReportsTheSpread(t *testing.T) {
	r := Run(context.Background(), mockProber{})

	c := find(r, "Nitro fleet")
	if c == nil {
		t.Fatal("no 'Nitro fleet' check in the report")
	}
	if c.Status != Pass {
		t.Fatalf("status = %v, want Pass", c.Status)
	}
	if c.Detail == "" {
		t.Error("passed with no detail — the generation spread is the only thing this " +
			"check contributes, so an empty detail makes it noise")
	}
	for _, want := range []string{"Nitro instance types", "v6:"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not mention %q", c.Detail, want)
		}
	}
}
