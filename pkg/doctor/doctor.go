// Package doctor implements `spawn doctor`: a read-only preflight that checks
// whether the current environment is ready to launch and manage a spore instance.
// Each check reports one of three states so a user (or their cloud admin) can see
// exactly which prerequisite is missing before the first launch.
package doctor

import (
	"context"
	"fmt"
	"strings"
)

// Status is the outcome of a single check.
type Status int

const (
	// Pass — the prerequisite is satisfied.
	Pass Status = iota
	// Warn — non-fatal: launch can proceed, but an optional/feature capability is
	// unavailable (e.g. no reaper backstop, no Route 53).
	Warn
	// Fail — a core prerequisite is missing; `spawn launch` would not succeed.
	Fail
	// Skip — the check couldn't run because a prerequisite check already failed
	// (e.g. can't test EC2 permissions if credentials are invalid).
	Skip
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "pass"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	case Skip:
		return "skip"
	default:
		return "unknown"
	}
}

// Symbol returns the leading glyph used in human output.
func (s Status) Symbol() string {
	switch s {
	case Pass:
		return "✓"
	case Warn:
		return "⚠"
	case Fail:
		return "✗"
	case Skip:
		return "–"
	default:
		return "?"
	}
}

// Check is the result of one preflight check.
type Check struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Detail  string `json:"detail,omitempty"` // e.g. resolved account/region, or the value found
	Fix     string `json:"fix,omitempty"`    // actionable hint shown on Warn/Fail
	Skipped bool   `json:"-"`                // internal: convenience alias for Status==Skip
}

// Report is the full set of checks plus a derived summary.
type Report struct {
	Checks []Check `json:"checks"`
}

// OK reports whether the environment is ready to launch: no Fail checks. Warns
// are acceptable (optional features), Skips only occur downstream of a Fail so
// they don't independently block.
func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return false
		}
	}
	return true
}

// Counts returns how many checks landed in each status.
func (r *Report) Counts() (pass, warn, fail, skip int) {
	for _, c := range r.Checks {
		switch c.Status {
		case Pass:
			pass++
		case Warn:
			warn++
		case Fail:
			fail++
		case Skip:
			skip++
		}
	}
	return
}

// Prober performs the individual environment probes. It is an interface so the
// runner can be unit-tested with a mock and the real implementation can wrap the
// AWS client. Every method is read-only. A probe returns (detail, error): a
// non-nil error becomes a Fail (or, for optional probes, a Warn) with the error
// text as the fix hint; the detail string is shown on success.
type Prober interface {
	// Local, no-AWS checks.
	SpawnVersion() string
	TruffleAvailable() (detail string, err error)
	AWSCLIAvailable() (detail string, err error)
	SSHKeyPresent() (detail string, err error)

	// Identity / region.
	Credentials(ctx context.Context) (accountID string, err error)
	ExpectedAccount() string // "" if the user didn't pin one (SPORE_ACCOUNT/--account)
	Region(ctx context.Context) (region string, err error)

	// Permission / resource probes (read-only AWS calls).
	EC2Describe(ctx context.Context) error
	EC2LaunchPermission(ctx context.Context) error // DryRun RunInstances
	IAMInstanceProfileAccess(ctx context.Context) error
	SporedRolePresent(ctx context.Context) (detail string, err error)
	VPCAndSubnet(ctx context.Context) (detail string, err error)
	SSMAvailable(ctx context.Context) (detail string, err error)

	// Optional features → Warn (not Fail) when unavailable.
	ReaperConfigured(ctx context.Context) (detail string, err error)
	Route53Available(ctx context.Context) (detail string, err error)
}

// Run executes every check in dependency order and returns the report. Checks
// that depend on a failed prerequisite are marked Skip rather than run (so a bad
// credential doesn't produce a cascade of confusing permission failures).
func Run(ctx context.Context, p Prober) *Report {
	r := &Report{}
	add := func(name string, status Status, detail, fix string) {
		r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail, Fix: fix, Skipped: status == Skip})
	}

	// --- Local checks (never skipped) ---
	add("spawn version", Pass, p.SpawnVersion(), "")

	if detail, err := p.TruffleAvailable(); err != nil {
		add("truffle installed", Warn, "", "install truffle for capacity discovery: "+errText(err))
	} else {
		add("truffle installed", Pass, detail, "")
	}

	cliOK := true
	if detail, err := p.AWSCLIAvailable(); err != nil {
		cliOK = false
		add("AWS CLI available", Warn, "", "install AWS CLI v2.32.0+ for `aws login`: "+errText(err))
	} else {
		add("AWS CLI available", Pass, detail, "")
	}
	_ = cliOK

	if detail, err := p.SSHKeyPresent(); err != nil {
		add("SSH key", Warn, "", "no default SSH key found; spawn will generate a managed one: "+errText(err))
	} else {
		add("SSH key", Pass, detail, "")
	}

	// --- Credentials gate: everything below depends on valid credentials ---
	acct, credErr := p.Credentials(ctx)
	if credErr != nil {
		add("AWS credentials", Fail, "", "run `aws login` (or configure a profile/keys): "+errText(credErr))
		// Everything AWS-dependent is skipped.
		for _, name := range []string{
			"account", "region", "EC2 describe permission", "EC2 launch permission",
			"IAM instance-profile permission", "spored role", "VPC & subnet",
			"Session Manager", "TTL reaper backstop", "Route 53 (DNS)",
		} {
			add(name, Skip, "", "requires valid credentials")
		}
		return r
	}
	add("AWS credentials", Pass, "identity resolved", "")

	// Account (+ optional expected-account match).
	if want := p.ExpectedAccount(); want != "" && want != acct {
		add("account", Fail, acct, fmt.Sprintf("resolved account %s does not match the expected account %s (SPORE_ACCOUNT/--account) — you may be pointed at the wrong account", acct, want))
	} else {
		detail := acct
		if want != "" {
			detail = acct + " (matches expected)"
		}
		add("account", Pass, detail, "")
	}

	// Region.
	region, regErr := p.Region(ctx)
	if regErr != nil {
		add("region", Fail, "", "set a region (--region, AWS_REGION, or profile default): "+errText(regErr))
	} else {
		add("region", Pass, region, "")
	}

	// Permission probes.
	checkErr := func(name string, err error, fix string) {
		if err != nil {
			add(name, Fail, "", fix+": "+errText(err))
		} else {
			add(name, Pass, "", "")
		}
	}
	checkErr("EC2 describe permission", p.EC2Describe(ctx), "grant ec2:Describe* (see the IAM baseline)")
	checkErr("EC2 launch permission", p.EC2LaunchPermission(ctx), "grant ec2:RunInstances (see the IAM baseline)")
	checkErr("IAM instance-profile permission", p.IAMInstanceProfileAccess(ctx), "grant iam:* on spored* + iam:PassRole (see the IAM baseline)")

	if detail, err := p.SporedRolePresent(ctx); err != nil {
		add("spored role", Warn, "", "spored instance profile not found yet; spawn will create it on first launch if IAM permits: "+errText(err))
	} else {
		add("spored role", Pass, detail, "")
	}

	if detail, err := p.VPCAndSubnet(ctx); err != nil {
		add("VPC & subnet", Fail, "", "no usable VPC/subnet in this region; create a default VPC or pass --subnet: "+errText(err))
	} else {
		add("VPC & subnet", Pass, detail, "")
	}

	if detail, err := p.SSMAvailable(ctx); err != nil {
		add("Session Manager", Warn, "", "SSM not reachable; `spawn connect` SSM fallback won't work: "+errText(err))
	} else {
		add("Session Manager", Pass, detail, "")
	}

	// Optional features → Warn when unavailable.
	if detail, err := p.ReaperConfigured(ctx); err != nil {
		add("TTL reaper backstop", Warn, "", "no out-of-band reaper configured; TTL is enforced in-instance by spored only (see docs/safety): "+errText(err))
	} else {
		add("TTL reaper backstop", Pass, detail, "")
	}

	if detail, err := p.Route53Available(ctx); err != nil {
		add("Route 53 (DNS)", Warn, "", "Route 53 access unavailable; --dns subdomains won't work (optional): "+errText(err))
	} else {
		add("Route 53 (DNS)", Pass, detail, "")
	}

	return r
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}
