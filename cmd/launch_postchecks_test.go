package cmd

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
)

// countingSteps records every call the post-launch checks make (#1296).
type countingSteps struct {
	password, ingress, ssh, verify int
	checks                         []sporedCheck
	ing                            aws.PortIngress
	ingErr                         error
}

func (c *countingSteps) steps() postLaunchSteps {
	return postLaunchSteps{
		waitPassword: func(context.Context) error { c.password++; return nil },
		sshIngress: func(context.Context) (aws.PortIngress, error) {
			c.ingress++
			return c.ing, c.ingErr
		},
		waitSSH: func(context.Context, string, time.Duration) error { c.ssh++; return nil },
		verifySpored: func(_ context.Context, check sporedCheck, _ time.Duration) error {
			c.verify++
			c.checks = append(c.checks, check)
			return nil
		},
	}
}

var (
	ingressOpen   = aws.PortIngress{Port: 22, Open: true, Groups: []string{"sg-ssh (ssh)"}}
	ingressClosed = aws.PortIngress{Port: 22, Open: false, Groups: []string{"sg-0abc (default)"}}
)

// runChecks drives runPostLaunchChecks with the plan `spawn launch` would build
// from --wait-for-ssh (set = given at all).
func runChecks(t *testing.T, targetOS string, set, value bool, ing aws.PortIngress) (*countingSteps, postLaunchOutcome, string) {
	t.Helper()
	c := &countingSteps{ing: ing}
	var w bytes.Buffer
	out := runPostLaunchChecks(context.Background(), &w, progress.NewQuietProgress(), c.steps(),
		resolvePostLaunchPlan(targetOS, set, value), targetOS, "203.0.113.10", "smoke", "i-0123")
	return c, out, w.String()
}

// The default Linux launch does not wait for SSH, and still verifies spored —
// the TTL safety net never depended on SSH (#1296). Both group states: an open
// port must not bring the wait back by default either.
func TestDefaultLinuxLaunchVerifiesSporedWithoutWaitingForSSH(t *testing.T) {
	for _, ing := range []aws.PortIngress{ingressOpen, ingressClosed} {
		c, out, _ := runChecks(t, "linux", false, false, ing)
		if c.ssh != 0 {
			t.Errorf("open=%v: default launch waited for SSH %d time(s); SSH is waited for only with --wait-for-ssh", ing.Open, c.ssh)
		}
		if c.verify != 1 || c.checks[0] != sporedCheckLinux {
			t.Errorf("open=%v: default launch verified spored %d time(s) (%v), want once with the Linux check", ing.Open, c.verify, c.checks)
		}
		if !out.sporedVerified {
			t.Errorf("open=%v: outcome does not report spored verified", ing.Open)
		}
	}
}

// Default launch into a closed default VPC: no wait, but say SSH can't reach it
// and that `spawn connect` uses SSM, and don't advertise an ssh line.
func TestDefaultLinuxLaunchSaysWhenPort22IsClosed(t *testing.T) {
	_, out, msg := runChecks(t, "linux", false, false, ingressClosed)
	if !out.sshClosed {
		t.Error("outcome does not record that port 22 is closed; the success summary would print an ssh line that times out")
	}
	for _, want := range []string{"sg-0abc (default)", "tcp/22", "spawn connect smoke"} {
		if !strings.Contains(msg, want) {
			t.Errorf("notice %q does not mention %q", msg, want)
		}
	}
}

// --wait-for-ssh with groups that admit no SSH: say so at once, naming the
// groups and how to proceed, and do not wait.
func TestRequestedSSHWithPort22ClosedIsRefusedImmediately(t *testing.T) {
	c, out, msg := runChecks(t, "linux", true, true, ingressClosed)
	if c.ssh != 0 {
		t.Errorf("waited for SSH %d time(s) on an instance whose groups admit no tcp/22; the answer is known before waiting", c.ssh)
	}
	if c.ingress != 1 {
		t.Errorf("read the security groups %d time(s), want 1", c.ingress)
	}
	for _, want := range []string{"Not waiting for SSH", "sg-0abc (default)", "spawn connect smoke", "--security-group-ids"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if c.verify != 1 || !out.sporedVerified {
		t.Errorf("spored verified %d time(s); refusing the SSH wait must not skip the safety-net check", c.verify)
	}
}

// --wait-for-ssh with a group that may admit SSH waits exactly as before. A
// group read that fails decides nothing, so it waits too.
func TestRequestedSSHWithPort22OpenWaits(t *testing.T) {
	c, _, _ := runChecks(t, "linux", true, true, ingressOpen)
	if c.ssh != 1 {
		t.Errorf("waited for SSH %d time(s), want 1: a group that allows tcp/22 must not be refused", c.ssh)
	}
	if c.verify != 1 {
		t.Errorf("spored verified %d time(s), want 1", c.verify)
	}

	c = &countingSteps{ingErr: errors.New("UnauthorizedOperation: ec2:DescribeSecurityGroups")}
	runPostLaunchChecks(context.Background(), &bytes.Buffer{}, progress.NewQuietProgress(), c.steps(),
		resolvePostLaunchPlan("linux", true, true), "linux", "203.0.113.10", "smoke", "i-0123")
	if c.ssh != 1 {
		t.Errorf("an unreadable security group refused the SSH wait (%d waits); no evidence is not a closed port", c.ssh)
	}
}

// An explicit --wait-for-ssh=false keeps its documented meaning on both OSes:
// skip the wait AND the spored check, and say the check was skipped.
func TestExplicitNoWaitForSSHSkipsBoth(t *testing.T) {
	for _, targetOS := range []string{"linux", "windows"} {
		c, out, msg := runChecks(t, targetOS, true, false, ingressOpen)
		if c.ssh+c.password+c.verify != 0 {
			t.Errorf("%s --wait-for-ssh=false: ssh=%d password=%d verify=%d, want all 0", targetOS, c.ssh, c.password, c.verify)
		}
		if out.sporedVerified || !strings.Contains(msg, aws.HeartbeatTag) {
			t.Errorf("%s --wait-for-ssh=false: skipped verification not announced (%q)", targetOS, msg)
		}
	}
}

// Windows is unchanged: by default (and with --wait-for-ssh) it waits for the
// Administrator password and verifies spored with the Windows check, and never
// probes or inspects port 22.
func TestWindowsPostLaunchUnchanged(t *testing.T) {
	for _, set := range []bool{false, true} {
		c, out, _ := runChecks(t, "windows", set, true, ingressClosed)
		if c.password != 1 {
			t.Errorf("set=%v: waited for the Windows password %d time(s), want 1", set, c.password)
		}
		if c.ssh != 0 || c.ingress != 0 {
			t.Errorf("set=%v: Windows probed SSH %d / read groups %d time(s), want 0", set, c.ssh, c.ingress)
		}
		if c.verify != 1 || c.checks[0] != sporedCheckWindows || !out.sporedVerified {
			t.Errorf("set=%v: spored verified %d time(s) with %v, want once with the Windows check", set, c.verify, c.checks)
		}
	}
}

// A spored verification failure is handed back for the launch to turn into its
// error — not swallowed.
func TestPostLaunchReturnsSporedFailure(t *testing.T) {
	c := &countingSteps{ing: ingressOpen}
	steps := c.steps()
	boom := errors.New("spored did not become ready")
	steps.verifySpored = func(context.Context, sporedCheck, time.Duration) error { return boom }
	out := runPostLaunchChecks(context.Background(), &bytes.Buffer{}, progress.NewQuietProgress(), steps,
		resolvePostLaunchPlan("linux", false, false), "linux", "203.0.113.10", "smoke", "i-0123")
	if !errors.Is(out.sporedErr, boom) || out.sporedVerified {
		t.Errorf("outcome = %+v, want the verification error and not verified", out)
	}
}

// The launch must take "was --wait-for-ssh given" from the flag set, not assume
// it: without it an explicit --wait-for-ssh=false would be indistinguishable
// from the default and the e2e/script opt-out would silently start verifying.
func TestRunLaunchRecordsWhetherWaitForSSHWasGiven(t *testing.T) {
	if launchCmd.Flags().Lookup("wait-for-ssh").DefValue != "false" {
		t.Error("--wait-for-ssh defaults to true; the default launch must not wait for SSH (#1296)")
	}
	// Anchored at line start, so a comment quoting the line cannot satisfy it.
	src := readSourceFile(t, "launch_single.go")
	if !regexp.MustCompile(`(?m)^\s*waitForSSHSet = cmd\.Flags\(\)\.Changed\("wait-for-ssh"\)\s*$`).MatchString(src) {
		t.Error("runLaunch does not record whether --wait-for-ssh was given")
	}
	if !regexp.MustCompile(`(?m)^\s*plan := resolvePostLaunchPlan\(config\.TargetOS, waitForSSHSet, waitForSSH\)\s*$`).MatchString(src) {
		t.Error("the launch does not build its post-launch plan from --wait-for-ssh")
	}
}
