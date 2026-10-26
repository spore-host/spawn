package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// Multi-instance launches — job arrays and MPI cohorts, parameter sweeps, burst
// — get the guarantees a single `spawn launch` has had since #1298 (#1305):
//
//  1. Every member's spored is verified, with the same check a single launch
//     makes (verifySporedReadyOn: the heartbeat tag, SSM as a second signal).
//     The members are verified in parallel against one shared deadline, so a
//     fleet waits about as long as one instance would, not five minutes per
//     member. A member whose spored never comes up is terminated, and reported,
//     unless --keep-on-error; its siblings are kept, because each of them is
//     verified and so enforcing its own TTL.
//  2. A failure after RunInstances that ends the command — a failed
//     ListInstances, a failed cohort, a burst that stopped half way — terminates
//     every member launched so far, unless --keep-on-error. One helper does this
//     for every path: fleetGuard.finish, deferred over the path's named error.
//
// The two differ on purpose. (1) is one member's outcome, and the launch goes
// on with the rest; (2) is the launch itself failing, after which nothing it
// started has been accounted for.

// fleetMember is one instance a multi-instance launch created.
type fleetMember struct {
	// label names the member in output: "parameter set 3", a job-array name.
	label      string
	instanceID string
	region     string
	// targetOS is the instance's OS ("windows" or anything else for Linux). It
	// picks the spored check, as on the single-instance path (#882).
	targetOS string
}

// fleetTerminator is what the guard needs to clean up. A seam: *aws.Client in
// a launch, a counting fake in tests.
type fleetTerminator interface {
	Terminate(ctx context.Context, region, instanceID string) error
}

// fleetVerifyFunc verifies one member's spored within timeout. nil means the
// launch asked for no verification (--wait-for-ssh=false, as on a single launch).
type fleetVerifyFunc func(ctx context.Context, m fleetMember, timeout time.Duration) error

// clientFleetVerifier verifies a member with the single-instance check.
func clientFleetVerifier(client *aws.Client) fleetVerifyFunc {
	return func(ctx context.Context, m fleetMember, timeout time.Duration) error {
		return verifySporedReadyOn(ctx, client, m.region, m.instanceID, sporedVerificationFor(true, m.targetOS), timeout)
	}
}

// fleetVerifyConcurrency bounds how many members are verified at once. Each
// verification polls DescribeInstances every sporedVerifyPollInterval, so this
// caps the fleet's polling at a couple of calls a second.
const fleetVerifyConcurrency = 16

// fleetVerifyFloor is the least time a member is given to show a heartbeat,
// even when it waited for a verification slot past the shared deadline: long
// enough for a final poll, so it is judged on evidence rather than on the queue.
const fleetVerifyFloor = 3 * sporedVerifyPollInterval

// fleetGuard tracks the members of one multi-instance launch, verifies them, and
// terminates what must not be left running. It is safe for concurrent use.
type fleetGuard struct {
	kind   string // "job array", "sweep", "burst": for messages
	w      io.Writer
	api    fleetTerminator
	verify fleetVerifyFunc
	keep   bool
	now    func() time.Time
	sem    chan struct{}

	mu       sync.Mutex
	members  []fleetMember
	gone     map[string]bool // terminate attempted (by the guard) or drained elsewhere: never again
	termFail map[string]bool // terminate attempted and failed: still running
	verified map[string]bool
	failed   map[string]error // verification failed
}

func newFleetGuard(kind string, w io.Writer, api fleetTerminator, verify fleetVerifyFunc, keep bool) *fleetGuard {
	return &fleetGuard{
		kind: kind, w: w, api: api, verify: verify, keep: keep, now: time.Now,
		sem:      make(chan struct{}, fleetVerifyConcurrency),
		gone:     map[string]bool{},
		termFail: map[string]bool{},
		verified: map[string]bool{},
		failed:   map[string]error{},
	}
}

// launched records a member as soon as RunInstances returns it.
func (g *fleetGuard) launched(m fleetMember) {
	if m.instanceID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, have := range g.members {
		if have.instanceID == m.instanceID {
			return
		}
	}
	g.members = append(g.members, m)
}

// markGone records that something else (the job-array drain) already
// terminated an instance, so the guard does not terminate it again.
func (g *fleetGuard) markGone(instanceID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gone[instanceID] = true
}

// live is every recorded member nothing has terminated yet.
func (g *fleetGuard) live() []fleetMember {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []fleetMember
	for _, m := range g.members {
		if !g.gone[m.instanceID] {
			out = append(out, m)
		}
	}
	return out
}

// terminate terminates one member, at most once over the guard's lifetime. It
// runs on a context detached from the launch's, so a launch that failed by
// running out of time can still clean up.
func (g *fleetGuard) terminate(ctx context.Context, m fleetMember) error {
	g.mu.Lock()
	if g.gone[m.instanceID] {
		g.mu.Unlock()
		return nil
	}
	g.gone[m.instanceID] = true
	g.mu.Unlock()

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), launchCleanupTimeout)
	defer cancel()
	if err := g.api.Terminate(cctx, m.region, m.instanceID); err != nil {
		g.mu.Lock()
		g.termFail[m.instanceID] = true
		g.mu.Unlock()
		fmt.Fprintf(g.w, "   terminating %s (%s) failed — TERMINATE IT MANUALLY (spawn terminate %s): %v\n",
			m.instanceID, m.label, m.instanceID, err)
		return err
	}
	return nil
}

// verifyOne verifies one member. start is when the member's clock started (its
// launch, or the start of a whole-fleet verification); it gets the OS's budget
// from then, and never less than fleetVerifyFloor once it holds a slot. On
// failure the member is terminated unless --keep-on-error. It returns the
// verification error, or nil once spored is confirmed (or verification is off).
func (g *fleetGuard) verifyOne(ctx context.Context, m fleetMember, start time.Time) error {
	g.launched(m) // a member being verified is one the guard answers for
	if g.verify == nil {
		return nil
	}
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return g.verifyFailed(ctx, m, ctx.Err())
	}
	timeout := sporedVerifyTimeout(sporedVerificationFor(true, m.targetOS)) - g.now().Sub(start)
	if timeout < fleetVerifyFloor {
		timeout = fleetVerifyFloor
	}
	err := g.verify(ctx, m, timeout)
	<-g.sem
	if err != nil {
		return g.verifyFailed(ctx, m, err)
	}
	g.mu.Lock()
	g.verified[m.instanceID] = true
	g.mu.Unlock()
	return nil
}

func (g *fleetGuard) verifyFailed(ctx context.Context, m fleetMember, err error) error {
	err = sporedVerificationError(err)
	g.mu.Lock()
	g.failed[m.instanceID] = err
	g.mu.Unlock()
	if g.keep {
		fmt.Fprintf(g.w, "⚠️  %s (%s): %v\n   Kept (--keep-on-error): it has NO TTL, idle or cost-limit enforcement until spored is confirmed.\n",
			m.label, m.instanceID, err)
		return err
	}
	fmt.Fprintf(g.w, "⚠️  %s (%s): %v\n   Terminating it; the other members are unaffected (--keep-on-error keeps it).\n",
		m.label, m.instanceID, err)
	_ = g.terminate(ctx, m)
	return err
}

// verifyAll verifies the given members in parallel against one shared start,
// and returns those whose spored was confirmed (all of them when verification
// is off). The rest have been terminated, or kept with --keep-on-error.
func (g *fleetGuard) verifyAll(ctx context.Context, members []fleetMember) []fleetMember {
	if g.verify == nil {
		if len(members) > 0 {
			fmt.Fprintf(g.w, "ℹ️  spored was not verified on the %d %s instance(s) (--wait-for-ssh=false). Once running it tags each with %s; "+
				"until then they have no TTL/idle safety net.\n", len(members), g.kind, aws.HeartbeatTag)
		}
		return members
	}
	if len(members) == 0 {
		return nil
	}
	fmt.Fprintf(g.w, "\n🔎 Verifying spored on %d instance(s) (up to %d min)...\n",
		len(members), int(sporedVerifyTimeout(sporedVerificationFor(true, members[0].targetOS)).Minutes()))
	start := g.now()
	ok := make([]bool, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Add(1)
		go func(i int, m fleetMember) {
			defer wg.Done()
			ok[i] = g.verifyOne(ctx, m, start) == nil
		}(i, m)
	}
	wg.Wait()
	var out []fleetMember
	for i, m := range members {
		if ok[i] {
			out = append(out, m)
		}
	}
	return out
}

// summary is the "N of M verified, K terminated" line every path prints.
func (g *fleetGuard) summary() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var term, kept, stuck []string
	for _, m := range g.members {
		if _, f := g.failed[m.instanceID]; !f {
			continue
		}
		switch {
		case g.termFail[m.instanceID]:
			stuck = append(stuck, m.instanceID)
		case g.gone[m.instanceID]:
			term = append(term, m.instanceID)
		default:
			kept = append(kept, m.instanceID)
		}
	}
	var b strings.Builder
	if g.verify == nil {
		fmt.Fprintf(&b, "spored not verified on %d instance(s) (--wait-for-ssh=false)", len(g.members))
	} else {
		fmt.Fprintf(&b, "spored verified on %d of %d instance(s)", len(g.verified), len(g.members))
	}
	if len(term) > 0 {
		fmt.Fprintf(&b, "; %d terminated because spored never came up (%s)", len(term), strings.Join(term, ", "))
	}
	if len(kept) > 0 {
		fmt.Fprintf(&b, "; %d kept unverified with --keep-on-error (%s)", len(kept), strings.Join(kept, ", "))
	}
	if len(stuck) > 0 {
		fmt.Fprintf(&b, "; %d FAILED to terminate and are still running (%s)", len(stuck), strings.Join(stuck, ", "))
	}
	return b.String()
}

// fleetPartialError is a launch that ended with some members failed and every
// surviving member verified (or verification off). finish passes it through:
// the survivors are each enforcing their own TTL, so tearing them down for a
// sibling's failure would destroy working members.
type fleetPartialError struct{ err error }

func (e *fleetPartialError) Error() string { return e.err.Error() }
func (e *fleetPartialError) Unwrap() error { return e.err }

// partial marks err as a member-level outcome rather than a launch failure.
func partial(err error) error {
	if err == nil {
		return nil
	}
	return &fleetPartialError{err: err}
}

// finish is deferred by each fleet path over its named error. A success or a
// partial outcome passes through. Any other error, once members exist,
// terminates every member nothing has terminated yet — each exactly once —
// unless --keep-on-error, which keeps them and says what is being kept.
func (g *fleetGuard) finish(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var p *fleetPartialError
	if errors.As(err, &p) {
		return err
	}
	live := g.live()
	if len(live) == 0 {
		return err
	}
	ids := make([]string, len(live))
	for i, m := range live {
		ids[i] = m.instanceID
	}
	if g.keep {
		fmt.Fprintf(g.w, "\n⚠️  The %s launch failed after creating %d instance(s); keeping them (--keep-on-error): %s\n"+
			"   Any whose spored is not confirmed has NO TTL, idle or cost-limit enforcement. Terminate them when\n"+
			"   done (spawn terminate <id>).\n", g.kind, len(live), strings.Join(ids, ", "))
		return fmt.Errorf("%s launch failed after creating %d instance(s); kept them (--keep-on-error): %w", g.kind, len(live), err)
	}
	fmt.Fprintf(g.w, "\n⚠️  The %s launch failed after creating %d instance(s); terminating them, so nothing is left\n"+
		"   billing without a safety net (--keep-on-error keeps them).\n", g.kind, len(live))
	var stuck []string
	for _, m := range live {
		if terr := g.terminate(ctx, m); terr != nil {
			stuck = append(stuck, m.instanceID)
		} else {
			fmt.Fprintf(g.w, "   terminated %s (%s)\n", m.instanceID, m.label)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("%s launch failed after creating %d instance(s); terminated %d, and %d are STILL RUNNING (%s): %w",
			g.kind, len(live), len(live)-len(stuck), len(stuck), strings.Join(stuck, ", "), err)
	}
	return fmt.Errorf("%s launch failed after creating %d instance(s); terminated them: %w", g.kind, len(live), err)
}

// launchFleetVerifier is the verifier for the fleets `spawn launch` starts (job
// arrays, sweeps). An explicit --wait-for-ssh=false skips spored verification,
// with the meaning it has on a single launch (resolvePostLaunchPlan).
func launchFleetVerifier(client *aws.Client) fleetVerifyFunc {
	if !resolvePostLaunchPlan("", waitForSSHSet, waitForSSH).verifySpored {
		return nil
	}
	return clientFleetVerifier(client)
}

// failure is the verification error recorded for a member, if any.
func (g *fleetGuard) failure(instanceID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failed[instanceID]
}
