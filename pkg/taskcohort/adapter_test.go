package taskcohort

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
)

// fakeLauncher is an in-memory LaunchAPI — no real AWS. It records launches and
// can fail launches into a given AZ (models per-AZ capacity exhaustion) or by
// worker name, so we can drive capacity-fallback deterministically. Mirrors
// mpicohort's fake, trimmed to the four methods taskcohort's LaunchAPI needs.
type fakeLauncher struct {
	mu       sync.Mutex
	launched map[string]aws.InstanceInfo // name → instance
	nextID   int
	failAZs  map[string]bool // launches into one of these AZs ICE
	started  []string        // instance IDs passed to StartInstance
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{launched: map[string]aws.InstanceInfo{}, failAZs: map[string]bool{}}
}

func (f *fakeLauncher) Launch(_ context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAZs[cfg.AvailabilityZone] {
		return nil, &aws.LaunchError{Code: "InsufficientInstanceCapacity"}
	}
	f.nextID++
	id := itoa(f.nextID)
	inst := aws.InstanceInfo{
		InstanceID: "i-" + id, Name: cfg.Name, InstanceType: cfg.InstanceType,
		State: "running", Region: cfg.Region, AvailabilityZone: cfg.AvailabilityZone,
		PrivateIP: "10.0.0." + id,
	}
	f.launched[cfg.Name] = inst
	return &aws.LaunchResult{
		InstanceID: inst.InstanceID, Name: cfg.Name, PrivateIP: inst.PrivateIP,
		AvailabilityZone: cfg.AvailabilityZone, State: "running",
	}, nil
}

func (f *fakeLauncher) Terminate(_ context.Context, _, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, in := range f.launched {
		if in.InstanceID == instanceID {
			delete(f.launched, name)
		}
	}
	return nil
}

func (f *fakeLauncher) StartInstance(_ context.Context, _, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, instanceID)
	return nil
}

func (f *fakeLauncher) ListInstances(_ context.Context, _, _ string) ([]aws.InstanceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]aws.InstanceInfo, 0, len(f.launched))
	for _, in := range f.launched {
		out = append(out, in)
	}
	return out, nil
}

func (f *fakeLauncher) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launched)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// poolBudget is a fast per-phase budget for tests: a plain worker pool has a nil
// Enroller (trivially enrolled) and nil Assembler (no barrier), so only the
// launch/running waits matter.
func poolBudget() cohort.PhaseBudget {
	return cohort.PhaseBudget{
		LaunchAcked:    2 * time.Second,
		Running:        2 * time.Second,
		Enrolled:       time.Second,
		CohortBarrier:  time.Second,
		CohortAssembly: time.Second,
	}
}

// buildPool constructs a partial cohort of n fungible workers over the given rung
// + AZ-fallback chain, wired to the taskcohort provider seam. minViable models
// "ask for n, accept as few as minViable" — the best-effort/eventual contract.
func buildPool(t *testing.T, client LaunchAPI, region string, n, minViable int, rung cohort.Rung, chain []cohort.Rung) (*cohort.Reconciler, cohort.Cohort) {
	t.Helper()
	base := aws.LaunchConfig{InstanceType: rung.InstanceType, Region: region}
	act := &Actuator{Client: client, Region: region, BaseConfig: base}
	obs := &Observer{Client: client, Region: region}

	members := make([]cohort.EntityIntent, 0, n)
	for i := 0; i < n; i++ {
		id := cohort.EntityID("pool-worker-" + itoa(i))
		intent, err := cohort.NewEntityIntent("pool", id, "g1", cohort.CohortID("pool-run-1"),
			cohort.RungPlacement{Rung: rung, Chain: chain}, "")
		if err != nil {
			t.Fatalf("NewEntityIntent(%s): %v", id, err)
		}
		members = append(members, intent)
	}
	c, err := cohort.NewPartialCohort(cohort.CohortID("pool-run-1"), members, poolBudget(), minViable, nil)
	if err != nil {
		t.Fatalf("NewPartialCohort: %v", err)
	}
	r := cohort.NewReconciler(act, obs, Classifier{}, nil, nil, nil)
	return r, c
}

// TestPool_AllWorkersComeUp is the happy path: a partial cohort of N fungible
// workers all launch and the pool reconciles Ready with N live instances — with
// cohort UNMODIFIED (NewPartialCohort + the taskcohort seam only).
func TestPool_AllWorkersComeUp(t *testing.T) {
	f := newFakeLauncher()
	rung := cohort.Rung{InstanceType: "c7i.large", AvailZone: "us-east-1a", CapacityModel: cohort.CapacityOnDemand}
	r, c := buildPool(t, f, "us-east-1", 8, 1, rung, []cohort.Rung{rung})

	out, err := r.Reconcile(context.Background(), c)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Ready {
		t.Fatalf("pool not Ready; records: %+v", out.Records)
	}
	if got := f.liveCount(); got != 8 {
		t.Fatalf("live workers = %d, want 8", got)
	}
}

// TestPool_BestEffortDegrades is the eventual-semantics contract: some workers
// can't get capacity, but the pool still comes up Ready as long as >= minViable
// workers launched. Fewer workers ⇒ lower parallelism, NOT failure — the property
// the issue (#70) demands for an interactive demo that must never hang.
//
// Two of three AZs are capacity-exhausted and the chain has no more rungs, so
// workers pinned to the dead AZs go terminal while the us-east-1a workers succeed.
// With minViable=1 the pool is Ready on any survivor.
func TestPool_BestEffortDegrades(t *testing.T) {
	f := newFakeLauncher()
	f.failAZs["us-east-1b"] = true
	f.failAZs["us-east-1c"] = true

	// Half the workers are pinned to a dead AZ (single-rung, no fallback), half to
	// the live one. The dead-AZ workers fault CapacityExhausted with an exhausted
	// chain → terminal; the live-AZ workers succeed. minViable=1 ⇒ Ready.
	region := "us-east-1"
	base := aws.LaunchConfig{InstanceType: "c7i.large", Region: region}
	act := &Actuator{Client: f, Region: region, BaseConfig: base}
	obs := &Observer{Client: f, Region: region}
	live := cohort.Rung{InstanceType: "c7i.large", AvailZone: "us-east-1a", CapacityModel: cohort.CapacityOnDemand}
	dead := cohort.Rung{InstanceType: "c7i.large", AvailZone: "us-east-1b", CapacityModel: cohort.CapacityOnDemand}

	var members []cohort.EntityIntent
	for i := 0; i < 6; i++ {
		rung := live
		if i%2 == 0 {
			rung = dead
		}
		id := cohort.EntityID("pool-worker-" + itoa(i))
		intent, err := cohort.NewEntityIntent("pool", id, "g1", cohort.CohortID("pool-run-1"),
			cohort.RungPlacement{Rung: rung, Chain: []cohort.Rung{rung}}, "")
		if err != nil {
			t.Fatalf("NewEntityIntent: %v", err)
		}
		members = append(members, intent)
	}
	c, err := cohort.NewPartialCohort(cohort.CohortID("pool-run-1"), members, poolBudget(), 1, nil)
	if err != nil {
		t.Fatalf("NewPartialCohort: %v", err)
	}
	r := cohort.NewReconciler(act, obs, Classifier{}, nil, nil, nil)

	out, err := r.Reconcile(context.Background(), c)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Ready {
		t.Fatalf("pool should be Ready with >=1 viable worker; records: %+v", out.Records)
	}
	// The three live-AZ workers came up; the three dead-AZ workers did not.
	if got := f.liveCount(); got != 3 {
		t.Fatalf("live workers = %d, want 3 (the us-east-1a half)", got)
	}
}

// TestPool_AZFallback proves per-entity capacity fallback: the primary AZ is
// exhausted, but each worker independently advances to the next rung's AZ and
// succeeds there. No collective placement-group invariant (contrast mpicohort), so
// every worker falls back on its own and the whole pool still comes up.
func TestPool_AZFallback(t *testing.T) {
	f := newFakeLauncher()
	f.failAZs["us-east-1a"] = true // primary dead; chain's second rung (1b) is live

	primary := cohort.Rung{InstanceType: "c7i.large", AvailZone: "us-east-1a", CapacityModel: cohort.CapacityOnDemand}
	fallback := cohort.Rung{InstanceType: "c7i.large", AvailZone: "us-east-1b", CapacityModel: cohort.CapacityOnDemand}
	chain := []cohort.Rung{primary, fallback}
	r, c := buildPool(t, f, "us-east-1", 4, 1, primary, chain)

	out, err := r.Reconcile(context.Background(), c)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Ready {
		t.Fatalf("pool not Ready after AZ fallback; records: %+v", out.Records)
	}
	if got := f.liveCount(); got != 4 {
		t.Fatalf("live workers = %d, want 4 (all fell back to us-east-1b)", got)
	}
}

// TestClassifier_CapacityIsFallback checks the fault mapping the fallback path
// depends on: capacity codes → FaultCapacityExhausted (advance the ladder),
// throttles → FaultThrottle (back off), everything else → terminal.
func TestClassifier_CapacityIsFallback(t *testing.T) {
	cases := map[string]cohort.FaultClass{
		"InsufficientInstanceCapacity": cohort.FaultCapacityExhausted,
		"InsufficientHostCapacity":     cohort.FaultCapacityExhausted,
		"MaxSpotInstanceCountExceeded": cohort.FaultCapacityExhausted,
		"RequestLimitExceeded":         cohort.FaultThrottle,
		"Throttling":                   cohort.FaultThrottle,
		"UnauthorizedOperation":        cohort.FaultTerminal,
		"":                             cohort.FaultTerminal,
	}
	clf := Classifier{}
	for code, want := range cases {
		got := clf.Classify(&aws.LaunchError{Code: code}).Class
		if got != want {
			t.Errorf("Classify(%q).Class = %v, want %v", code, got, want)
		}
	}
	if clf.Classify(nil).Class != cohort.FaultRetryableConsistency {
		t.Errorf("Classify(nil) should be retryable-consistency")
	}
}
