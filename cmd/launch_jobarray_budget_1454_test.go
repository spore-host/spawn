package cmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/mpicohort"
)

// launchRejection is an error as aws.Client.Launch returns it: a
// *aws.LaunchError carrying AWS's code, with AWS's message in its text.
type launchRejection struct{ code, msg string }

func (e launchRejection) Error() string {
	return fmt.Sprintf("failed to launch instance: %s: api error %s: %s", e.code, e.code, e.msg)
}

func (e launchRejection) Unwrap() error { return &aws.LaunchError{Code: e.code} }

const profileRejectionMessage = "Value (spawn-instance-ja-1) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name"

// arrayEC2 is the mpicohort.LaunchAPI as EC2 behaves for a job array, for
// #1454.
//
//   - chain is the rung zones in order and perAttempt what one Launch's own
//     calls take. A Launch whose context leaves less than the #572 retry
//     window plus perAttempt for this rung and every rung after it fails as
//     Launch does when the deadline ends the retry: a member's context has to
//     hold the walk it may still make, with the retry in it. Zero perAttempt
//     disables the check.
//   - ice names zones out of capacity; reject names members whose launch EC2
//     rejects for the instance profile.
type arrayEC2 struct {
	mpicohort.LaunchAPI // unused methods

	chain      []string
	perAttempt time.Duration
	ice        map[string]bool
	reject     map[string]bool

	mu       sync.Mutex
	launches int
	pgs      int
	insts    map[string]aws.InstanceInfo
}

func newArrayEC2(chain ...string) *arrayEC2 {
	return &arrayEC2{chain: chain, ice: map[string]bool{}, reject: map[string]bool{}, insts: map[string]aws.InstanceInfo{}}
}

func (f *arrayEC2) Launch(ctx context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	f.mu.Lock()
	f.launches++
	f.mu.Unlock()
	if f.perAttempt > 0 {
		left := len(f.chain) - slices.Index(f.chain, cfg.AvailabilityZone)
		need := aws.InstanceProfilePropagationRetryWindow() + time.Duration(left)*f.perAttempt
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < need {
			return nil, launchRejection{code: "InvalidParameterValue", msg: profileRejectionMessage + " (retry stopped: context deadline exceeded)"}
		}
	}
	if f.reject[cfg.Name] {
		return nil, launchRejection{code: "InvalidParameterValue", msg: profileRejectionMessage}
	}
	if f.ice[cfg.AvailabilityZone] {
		return nil, launchRejection{code: "InsufficientInstanceCapacity", msg: "We currently do not have sufficient capacity in the Availability Zone you requested"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("i-%04d", len(f.insts)+1)
	f.insts[id] = aws.InstanceInfo{InstanceID: id, Name: cfg.Name, State: "running", JobArrayID: "ja-1", AvailabilityZone: cfg.AvailabilityZone}
	return &aws.LaunchResult{InstanceID: id, Name: cfg.Name, State: "running"}, nil
}

func (f *arrayEC2) Terminate(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.insts[id]
	in.State = "terminated"
	f.insts[id] = in
	return nil
}

func (f *arrayEC2) ListInstances(context.Context, string, string) ([]aws.InstanceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []aws.InstanceInfo
	for _, in := range f.insts {
		if in.State != "terminated" {
			out = append(out, in)
		}
	}
	return out, nil
}

func (f *arrayEC2) GetInstancePublicIP(context.Context, string, string) (string, error) {
	return "", nil
}

func (f *arrayEC2) CreatePlacementGroup(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pgs++
	return nil
}

func (f *arrayEC2) DeletePlacementGroup(context.Context, string, string) error { return nil }

// arrayTestCohort is reconcileArrayMembers' cohort over ec2: the Actuator,
// Observer, Classifier, budget and cohort a launch builds, with members w-0..
// w-(n-1) placed on the chain's first rung.
type arrayTestCohort struct {
	ids       []cohort.EntityID
	guard     *fleetGuard
	reconcile func(context.Context) (cohort.Outcome, error)
}

func newArrayTestCohort(t *testing.T, ec2 *arrayEC2, spec cohortSpec, base *aws.LaunchConfig, n, minViable int) arrayTestCohort {
	t.Helper()
	chain := make([]cohort.Rung, len(ec2.chain))
	for i, z := range ec2.chain {
		chain[i] = cohort.Rung{InstanceType: base.InstanceType, AvailZone: z, CapacityModel: cohort.CapacityOnDemand}
	}
	members := make([]cohort.EntityIntent, n)
	ids := make([]cohort.EntityID, n)
	for i := range members {
		ids[i] = cohort.EntityID(fmt.Sprintf("w-%d", i))
		intent, err := cohort.NewEntityIntent("arr", ids[i], "g1", "ja-1",
			cohort.RungPlacement{Rung: chain[0], Chain: chain}, memberIdempotencyToken("ja-1", ids[i]))
		if err != nil {
			t.Fatal(err)
		}
		members[i] = intent
	}
	guard := newFleetGuard("job array", io.Discard, ec2, sporedUp(), false)
	act := newArrayActuator(ec2, guard, base, nil)
	r := cohort.NewReconciler(act, &mpicohort.Observer{Client: ec2, Region: base.Region}, mpicohort.Classifier{}, nil, nil, nil)
	c, err := buildCohort(spec, "ja-1", members, minViable, cohortBudget(spec, arrayCohortLaunch(chain, base)))
	if err != nil {
		t.Fatal(err)
	}
	return arrayTestCohort{ids: ids, guard: guard, reconcile: func(ctx context.Context) (cohort.Outcome, error) { return r.Reconcile(ctx, c) }}
}

// #1454. Each member's Launch retries the #572 rejection for
// aws.InstanceProfilePropagationRetryWindow, around its own calls, on every
// rung its launch context holds. The job array's launch budget must hold all
// of it: a flat 30s, shorter than the retry window alone, cut members off
// mid-retry, as cohort's 15s default did to pool workers in #1448.
func TestJobArrayLaunchBudgetHoldsTheProfileRetryWindow(t *testing.T) {
	// What one Launch's own calls take, as far as the budget is concerned: most
	// of the allowance for them, with the zone's placement group wait on top.
	const perAttempt = 45 * time.Second
	for _, tc := range []struct {
		name         string
		spec         cohortSpec
		pgPrefix     string
		chain, ice   []string
		n, minViable int
		launches     int
	}{
		// A plain array on one rung, members placing on their own.
		{name: "array, one zone", chain: []string{"us-east-1a"}, n: 3, minViable: 1, launches: 3},
		// A member walks the chain in one context: three zones out of capacity
		// and the retry still fits in front of the fourth.
		{name: "array walks four zones", chain: []string{"us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d"},
			ice: []string{"us-east-1a", "us-east-1b", "us-east-1c"}, n: 2, minViable: 1, launches: 8},
		// Every member required: the cohort places as a unit, a round per rung.
		{name: "array of all-required members", chain: []string{"us-east-1a", "us-east-1b"},
			ice: []string{"us-east-1a"}, n: 3, minViable: 3, launches: 6},
		// An MPI cluster with a placement group per zone: each attempt first
		// waits for its zone's group.
		{name: "MPI with per-zone placement groups", spec: cohortSpec{mpi: true}, pgPrefix: "spawn-mpi-ja",
			chain: []string{"us-east-1a", "us-east-1b"}, ice: []string{"us-east-1a"}, n: 2, launches: 4},
		// A one-node MPI cluster does not place as a unit, so it walks the chain
		// in one context like a plain array's member.
		{name: "one-node MPI walks four zones", spec: cohortSpec{mpi: true},
			chain: []string{"us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d"},
			ice:   []string{"us-east-1a", "us-east-1b", "us-east-1c"}, n: 1, launches: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ec2 := newArrayEC2(tc.chain...)
			ec2.perAttempt = perAttempt
			if tc.pgPrefix != "" {
				ec2.perAttempt += aws.PlacementGroupAvailableWait()
			}
			for _, z := range tc.ice {
				ec2.ice[z] = true
			}
			base := &aws.LaunchConfig{Region: "us-east-1", InstanceType: "c8g.8xlarge", PlacementGroupPrefix: tc.pgPrefix}
			ac := newArrayTestCohort(t, ec2, tc.spec, base, tc.n, tc.minViable)
			outcome, err := ac.reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range ac.ids {
				if rec := outcome.Records[id]; !rec.Succeeded() {
					t.Errorf("%s was cut off before its profile retry could finish: %s", id, cohortMemberReason(rec))
				}
			}
			if ec2.launches != tc.launches {
				t.Errorf("%d Launch calls, want %d", ec2.launches, tc.launches)
			}
			if tc.pgPrefix != "" && ec2.pgs == 0 {
				t.Error("no placement group was created, so the placement group wait was not exercised")
			}
		})
	}
}

// #1454. A member that fails says why, in AWS's words, and so does every
// member cancelled around it. Summary() printed "terminal/InvalidParameterValue
// at phase=launch-acked" with no message.
func TestJobArrayFailureNamesTheAWSError(t *testing.T) {
	for _, tc := range []struct {
		name         string
		spec         cohortSpec
		n, minViable int
	}{
		{name: "MPI", spec: cohortSpec{mpi: true}, n: 3},
		{name: "array", n: 3, minViable: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ec2 := newArrayEC2("us-east-1a")
			ec2.reject["w-1"] = true
			base := &aws.LaunchConfig{Region: "us-east-1", InstanceType: "c8g.8xlarge"}
			ac := newArrayTestCohort(t, ec2, tc.spec, base, tc.n, tc.minViable)
			_, err := runArrayCohort(context.Background(), ec2, ac.guard, ac.reconcile, arrayRunFor(ac.ids, tc.spec.mpi, tc.minViable))
			if err == nil {
				t.Fatal("a cohort with a rejected required member launched")
			}
			const reason = "terminal/InvalidParameterValue: failed to launch instance: InvalidParameterValue: api error InvalidParameterValue: " + profileRejectionMessage
			lines := strings.Split(err.Error(), "\n")
			for _, id := range ac.ids {
				var line string
				for _, l := range lines {
					if strings.HasPrefix(strings.TrimSpace(l), string(id)+":") {
						line = l
					}
				}
				if !strings.Contains(line, reason) {
					t.Errorf("%s's line does not carry the AWS error %q:\n%s", id, reason, err)
				}
			}
			if !strings.Contains(err.Error(), "cohort-cancelled") {
				t.Errorf("no member was reported cancelled around the culprit, so that path was not exercised:\n%s", err)
			}
		})
	}
}
