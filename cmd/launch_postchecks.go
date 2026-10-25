package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
)

// postLaunchPlan is what a launch waits for once the instance is running
// (spawn#1296).
//
// SSM is the default way into a Linux spore: spawn creates no security group
// for a plain Linux launch, so the instance gets the VPC's default group, which
// in an unmodified default VPC has no port-22 rule. Waiting for SSH by default
// therefore spent two minutes on every such launch and then printed an error
// about a healthy instance. SSH is now waited for only when asked.
//
// spored is verified regardless, because it is the TTL/idle safety net and its
// verification never needed SSH (heartbeat tag, SSM as a second signal). The
// one exception is an explicit --wait-for-ssh=false, which keeps the meaning it
// has always documented — "skip both" — for the scripts and tests that pass it
// to return as soon as RunInstances does.
type postLaunchPlan struct {
	// waitSSH probes port 22 (Linux only).
	waitSSH bool
	// waitPassword waits for the Windows Administrator password (Windows only).
	waitPassword bool
	// verifySpored confirms spored came up.
	verifySpored bool
}

// resolvePostLaunchPlan turns --wait-for-ssh into a plan. set is whether the
// flag was given at all, value what it was given as.
//
//   - not given: Linux verifies spored and does not wait for SSH; Windows waits
//     for the password and verifies spored, exactly as before.
//   - --wait-for-ssh: as before on both — wait, then verify.
//   - --wait-for-ssh=false: as before on both — skip the wait and verification.
func resolvePostLaunchPlan(targetOS string, set, value bool) postLaunchPlan {
	optedOut := set && !value
	if isWindowsTarget(targetOS) {
		return postLaunchPlan{waitPassword: !optedOut, verifySpored: !optedOut}
	}
	return postLaunchPlan{waitSSH: set && value, verifySpored: !optedOut}
}

// postLaunchSteps are the calls the post-launch checks make. A seam: the launch
// wires them to AWS and the network, tests count them.
type postLaunchSteps struct {
	waitPassword func(ctx context.Context) error
	sshIngress   func(ctx context.Context) (aws.PortIngress, error)
	waitSSH      func(ctx context.Context, host string, timeout time.Duration) error
	verifySpored func(ctx context.Context, check sporedCheck, timeout time.Duration) error
}

// clientPostLaunchSteps wires postLaunchSteps to AWS for one instance.
func clientPostLaunchSteps(client *aws.Client, region, instanceID string) postLaunchSteps {
	return postLaunchSteps{
		waitPassword: func(ctx context.Context) error {
			_, err := client.WaitForPasswordData(ctx, region, instanceID, 12*time.Minute)
			return err
		},
		sshIngress: func(ctx context.Context) (aws.PortIngress, error) {
			return client.InstancePortIngress(ctx, region, instanceID, 22)
		},
		waitSSH: waitForSSHReady,
		verifySpored: func(ctx context.Context, check sporedCheck, timeout time.Duration) error {
			return verifySporedReadyOn(ctx, client, region, instanceID, check, timeout)
		},
	}
}

// postLaunchOutcome is what the checks found.
type postLaunchOutcome struct {
	sporedVerified bool
	// sporedErr is non-nil when verification ran and failed; the caller turns
	// it into the launch's error (sporedVerificationFailure).
	sporedErr error
	// sshClosed is set when the attached security groups were read and none
	// can admit port 22, so an `ssh` hint would be wrong.
	sshClosed bool
}

// runPostLaunchChecks is steps 10 and 10b of a launch: wait for the instance
// to be usable (SSH on Linux when asked, the password on Windows), then verify
// spored.
func runPostLaunchChecks(ctx context.Context, w io.Writer, prog *progress.Progress, steps postLaunchSteps,
	plan postLaunchPlan, targetOS, publicIP, name, instanceID string) postLaunchOutcome {
	var out postLaunchOutcome

	// Windows: SSH (22) won't answer until late and the real "usable" signal is
	// the Administrator password becoming available (after EC2Launch runs,
	// post-Sysprep), so wait on that instead of probing port 22 (#95).
	if isWindowsTarget(targetOS) {
		prog.Start("Waiting for Windows (password available, up to 12 min)")
		if plan.waitPassword {
			// Best-effort: don't fail the launch if it's slow — the instance is
			// up and self-managing via spored.
			if err := steps.waitPassword(ctx); err != nil {
				fmt.Fprintf(w, "\n⚠️  Windows is still finishing first boot (Sysprep). The instance is up; `spawn connect %s` will wait for the password.\n", name)
			}
		}
		prog.Complete("Waiting for Windows (password available, up to 12 min)")
	} else {
		out.sshClosed = linuxSSHStep(ctx, w, prog, steps, plan, publicIP, name)
	}

	// Step 10b: Verify spored actually came up (#50). The bootstrap installs
	// spored asynchronously via cloud-init, so a failed install (bad download,
	// checksum mismatch, arch/network) is invisible to `spawn launch` — leaving a
	// "running" instance with NO lifecycle agent: no TTL enforcement, no idle stop,
	// no completion handling. That's a cost-control hole (a TTL-less zombie). So
	// confirm spored is up — by its heartbeat tag, with SSM as a second signal
	// (#1053) — and fail loudly if not. It does not depend on SSH (#1296); only
	// an explicit --wait-for-ssh=false skips it, and says so (#878).
	//
	// Decided by the INSTANCE's OS (targetOS), never by plat.OS — that is the
	// workstation's runtime.GOOS, and using it is #882. See sporedVerificationFor.
	if check := sporedVerificationFor(plan.verifySpored, targetOS); check != sporedCheckNone {
		timeout := sporedVerifyTimeout(check)
		// One label for Start/Error/Complete: progress matches steps by name (#739).
		step := fmt.Sprintf("Verifying spored agent (up to %d min)", int(timeout.Minutes()))
		prog.Start(step)
		if err := steps.verifySpored(ctx, check, timeout); err != nil {
			prog.Error(step, err)
			out.sporedErr = err
			return out
		}
		prog.Complete(step)
		out.sporedVerified = true
	} else {
		fmt.Fprintf(w, "ℹ️  spored was not verified (--wait-for-ssh=false). Once running it tags %s with %s; "+
			"until then the instance has no TTL/idle safety net.\n", instanceID, aws.HeartbeatTag)
	}
	return out
}

// linuxSSHStep is the Linux half of step 10. It reports whether port 22 is
// known to be closed by the security groups.
//
// The groups are read first, on every launch: it is one read-only call, and it
// is what lets a requested wait be refused before it starts rather than after
// two minutes, and the default launch stop advertising `ssh` for an instance
// whose groups cannot admit it. A failed read decides nothing — the wait goes
// ahead as before.
func linuxSSHStep(ctx context.Context, w io.Writer, prog *progress.Progress, steps postLaunchSteps,
	plan postLaunchPlan, publicIP, name string) bool {
	prog.Start("Waiting for SSH")
	if publicIP == "" {
		// No public address to probe. Previously this skipped the wait and still
		// reported ✅ (#740). SSM-only access is a legitimate configuration.
		prog.Skip("Waiting for SSH")
		if plan.waitSSH {
			fmt.Fprintf(w, "ℹ️  Skipping the SSH readiness check: the instance has no public IP (SSM-only access). Use `spawn connect %s`.\n", name)
		}
		return false
	}

	ing, ierr := steps.sshIngress(ctx)
	closed := ierr == nil && !ing.Open

	switch {
	case closed && plan.waitSSH:
		prog.Error("Waiting for SSH", fmt.Errorf("no security group attached to the instance allows inbound tcp/22"))
		fmt.Fprint(w, sshClosedNotice(ing.Groups, name, true))
	case closed:
		prog.Skip("Waiting for SSH")
		fmt.Fprint(w, sshClosedNotice(ing.Groups, name, false))
	case !plan.waitSSH:
		prog.Skip("Waiting for SSH")
	default:
		if err := steps.waitSSH(ctx, publicIP, 2*time.Minute); err != nil {
			// NOT fatal: the instance is running and spored is enforcing its own
			// TTL/idle/cost limits, so failing the launch here would destroy a
			// working spore over a connectivity problem. But it must be SAID
			// (#740).
			prog.Error("Waiting for SSH", err)
			fmt.Fprintf(w,
				"\n⚠️  SSH never became reachable (%v)\n"+
					"   The instance is up and spored is enforcing its lifecycle. Check the\n"+
					"   security group's port-22 ingress, or use `spawn connect %s`, which\n"+
					"   uses SSM when SSH is unavailable.\n",
				err, name)
		} else {
			prog.Complete("Waiting for SSH")
		}
	}
	return closed
}

// sshClosedNotice explains that the instance's security groups admit no SSH,
// naming them and the ways forward. requested is whether --wait-for-ssh asked
// for SSH (and so is not being waited for).
func sshClosedNotice(groups []string, name string, requested bool) string {
	var b strings.Builder
	if requested {
		b.WriteString("\n⚠️  Not waiting for SSH: ")
	} else {
		b.WriteString("ℹ️  ")
	}
	fmt.Fprintf(&b, "no security group on this instance allows inbound tcp/22 (%s), so SSH cannot reach it.\n", strings.Join(groups, ", "))
	fmt.Fprintf(&b, "   `spawn connect %s` uses SSM Session Manager instead and needs no open port.\n", name)
	b.WriteString("   For SSH, launch with --security-group-ids naming a group that allows tcp/22 from your\n" +
		"   address (or a --subnet-id whose VPC's default group does), or add that rule.\n")
	return b.String()
}
