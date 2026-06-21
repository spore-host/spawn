package mpicohort

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
)

// fakeLauncher is an in-memory LaunchAPI — no real AWS. It records launches and
// can be told to fail specific entity names with a given AWS error code, so we
// can drive capacity-fallback and barrier behavior deterministically.
type fakeLauncher struct {
	mu        sync.Mutex
	launched  map[string]aws.InstanceInfo // name → instance
	nextID    int
	failCode  map[string]string // name → AWS error code to return on Launch
	failOnce  map[string]bool   // if true, fail only the first launch of that name
	failAZ    string            // if set, any launch into this AZ ICEs (models per-AZ capacity)
	launchLog []launchRec
}

type launchRec struct {
	name         string
	instanceType string
	az           string
	userData     string
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{
		launched: map[string]aws.InstanceInfo{},
		failCode: map[string]string{},
		failOnce: map[string]bool{},
	}
}

func (f *fakeLauncher) Launch(_ context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchLog = append(f.launchLog, launchRec{cfg.Name, cfg.InstanceType, cfg.AvailabilityZone, cfg.UserData})

	if f.failAZ != "" && cfg.AvailabilityZone == f.failAZ {
		return nil, awsLaunchError("InsufficientInstanceCapacity")
	}
	if code, bad := f.failCode[cfg.Name]; bad {
		if f.failOnce[cfg.Name] {
			delete(f.failCode, cfg.Name) // subsequent attempts (next rung) succeed
		}
		return nil, awsLaunchError(code)
	}

	f.nextID++
	id := "i-" + itoa(f.nextID)
	inst := aws.InstanceInfo{
		InstanceID: id, Name: cfg.Name, InstanceType: cfg.InstanceType,
		State: "running", Region: cfg.Region, AvailabilityZone: cfg.AvailabilityZone,
		PrivateIP: "10.0.0." + itoa(f.nextID),
	}
	f.launched[cfg.Name] = inst
	return &aws.LaunchResult{
		InstanceID: id, Name: cfg.Name, PrivateIP: inst.PrivateIP,
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

func (f *fakeLauncher) StopInstance(_ context.Context, _, _ string, _ bool) error { return nil }
func (f *fakeLauncher) StartInstance(_ context.Context, _, _ string) error        { return nil }

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

// awsLaunchError builds an *aws.LaunchError with a given code, the way spawn's
// newLaunchError would. We construct it via the exported error path by wrapping
// a smithy-shaped error is overkill for the spike — instead rely on the Code
// field the Classifier reads. Since LaunchError.Code is exported, build directly.
func awsLaunchError(code string) error {
	return &aws.LaunchError{Code: code}
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

func fastBudget() cohort.PhaseBudget {
	return cohort.PhaseBudget{
		LaunchAcked: 3 * time.Second, Running: 3 * time.Second, Enrolled: 3 * time.Second,
		CohortBarrier: 3 * time.Second, CohortAssembly: 3 * time.Second,
	}
}

func newReconciler(f *fakeLauncher, asm cohort.Assembler) *cohort.Reconciler {
	return cohort.NewReconciler(
		&Actuator{Client: f, Region: "us-east-1", BaseConfig: aws.LaunchConfig{AMI: "ami-test"}},
		&Observer{Client: f, Region: "us-east-1"},
		Classifier{},
		Enroller{},
		asm,
		nil,
	)
}

// mpiCohort builds an N-member all-or-nothing MPI cohort, each node on the given
// rung with the given fallback chain.
func mpiCohort(n int, rung cohort.Rung, chain []cohort.Rung) cohort.Cohort {
	var members []cohort.EntityIntent
	for i := 0; i < n; i++ {
		m, _ := cohort.NewEntityIntent("mpi-cluster", cohort.EntityID("node-"+itoa(i)),
			"g1", "c-mpi", cohort.RungPlacement{Rung: rung, Chain: chain}, "")
		members = append(members, m)
	}
	c, _ := cohort.NewMPICohort("c-mpi", members, fastBudget())
	return c
}

// TestSpike_MPI_AllUp_Assembles: the happy path — 4 named nodes all come up,
// the barrier is satisfied, and Assemble runs ONCE over all 4 with their IPs.
func TestSpike_MPI_AllUp_Assembles(t *testing.T) {
	f := newFakeLauncher()
	var assembledWith []cohort.Observation
	asm := Assembler{WireUp: func(_ context.Context, members []cohort.Observation) error {
		assembledWith = members
		return nil
	}}
	r := newReconciler(f, asm)

	rung := cohort.Rung{InstanceType: "p5.48xlarge", AvailZone: "us-east-1a"}
	out, err := r.Reconcile(context.Background(), mpiCohort(4, rung, nil))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Ready {
		t.Fatalf("MPI cohort not Ready: %+v", out.Records)
	}
	if len(assembledWith) != 4 {
		t.Errorf("Assemble saw %d members, want 4", len(assembledWith))
	}
	for _, m := range assembledWith {
		if m.Address == "" {
			t.Errorf("member %s has no private IP for hostfile wire-up", m.ID)
		}
	}
}

// TestSpike_MPI_CollectiveFallback_PreservesAZ is the #5 proof: capacity is
// exhausted in AZ-a but available in AZ-b. The cohort advances its SHARED rung
// from a→b AS A UNIT, and ALL nodes land in the same AZ (b) — the placement-group
// invariant a per-entity fallback would have broken. This is the capability
// spawn's launchJobArray lacks AND the gap the collective-placement work closed.
func TestSpike_MPI_CollectiveFallback_PreservesAZ(t *testing.T) {
	f := newFakeLauncher()
	f.failAZ = "us-east-1a" // no p5 capacity in AZ-a; AZ-b is fine

	r := newReconciler(f, Assembler{})
	rung0 := cohort.Rung{InstanceType: "p5.48xlarge", AvailZone: "us-east-1a"}
	rung1 := cohort.Rung{InstanceType: "p5.48xlarge", AvailZone: "us-east-1b"}
	chain := []cohort.Rung{rung0, rung1}

	out, err := r.Reconcile(context.Background(), mpiCohort(4, rung0, chain))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Ready {
		t.Fatalf("cohort should recover by moving as a unit to AZ-b, not Ready")
	}

	// THE INVARIANT: every launched node is in the same AZ (us-east-1b). A
	// per-entity fallback would have left some in a and some in b — breaking the PG.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.launched) != 4 {
		t.Fatalf("want 4 live nodes, got %d", len(f.launched))
	}
	for name, in := range f.launched {
		if in.AvailabilityZone != "us-east-1b" {
			t.Errorf("%s landed in AZ %q — collective fallback must put ALL nodes in one AZ (us-east-1b)", name, in.AvailabilityZone)
		}
	}
}

// TestSpike_MPI_FastFailDrains: node-1's rung chain is exhausted (ICE with no
// fallback) → the whole cohort fast-fails and survivors are drained. Asserts the
// leak-free drain cohort gives us (vs launchJobArray's best-effort `_ = Terminate`).
func TestSpike_MPI_FastFailDrains(t *testing.T) {
	f := newFakeLauncher()
	f.failCode["node-1"] = "InsufficientInstanceCapacity" // permanent: no failOnce

	r := newReconciler(f, Assembler{})
	rung := cohort.Rung{InstanceType: "p5.48xlarge", AvailZone: "us-east-1a"}
	out, err := r.Reconcile(context.Background(), mpiCohort(4, rung, nil)) // no chain → exhausted on first ICE
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Ready {
		t.Fatal("cohort with an unsatisfiable node must not be Ready")
	}
	// node-1 is the culprit (Terminal); survivors are CohortCancelled.
	if rec := out.Records["node-1"]; rec.Terminal == nil {
		t.Errorf("node-1 should carry a Terminal fault")
	}
	// Drain: the reconciler tears down survivors so none is left billing. Give the
	// drain a beat (it runs in Reconcile before returning, so it's already done).
	if live := f.liveCount(); live != 0 {
		t.Errorf("drain incomplete: %d instances still live after fast-fail (cost leak)", live)
	}
}

// TestActuator_ConfigsLookup verifies the production seam: when Actuator.Configs
// has a per-entity LaunchConfig (carrying that member's MPI user-data), Launch
// uses it; an unmapped EntityID falls back to BaseConfig (the spike path).
func TestActuator_ConfigsLookup(t *testing.T) {
	f := newFakeLauncher()
	act := &Actuator{
		Client:     f,
		Region:     "us-east-1",
		BaseConfig: aws.LaunchConfig{UserData: "BASE-UD"},
		Configs: map[cohort.EntityID]aws.LaunchConfig{
			"node-0": {UserData: "PER-INDEX-UD-0"},
		},
	}

	rung := cohort.RungPlacement{Rung: cohort.Rung{InstanceType: "c5n.18xlarge", AvailZone: "us-east-1a"}}

	// Mapped ID → uses the per-entity config's user-data.
	mapped, _ := cohort.NewEntityIntent("c", "node-0", "g1", "c1", rung, "")
	if _, err := act.Launch(context.Background(), mapped); err != nil {
		t.Fatalf("launch node-0: %v", err)
	}
	// Unmapped ID → falls back to BaseConfig's user-data.
	unmapped, _ := cohort.NewEntityIntent("c", "node-9", "g1", "c1", rung, "")
	if _, err := act.Launch(context.Background(), unmapped); err != nil {
		t.Fatalf("launch node-9: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	got := map[string]string{}
	for _, r := range f.launchLog {
		got[r.name] = r.userData
	}
	if got["node-0"] != "PER-INDEX-UD-0" {
		t.Errorf("node-0 user-data = %q, want PER-INDEX-UD-0 (per-entity config)", got["node-0"])
	}
	if got["node-9"] != "BASE-UD" {
		t.Errorf("node-9 user-data = %q, want BASE-UD (BaseConfig fallback)", got["node-9"])
	}
}
