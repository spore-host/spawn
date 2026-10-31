package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	smithy "github.com/aws/smithy-go"
	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
)

// poolEC2 is the taskcohort.LaunchAPI as EC2 behaves for a pool, for #1448.
//
//   - Idempotency: RunInstances remembers each ClientToken with the request it
//     came with, rejected ones included. The same token with the same request
//     returns the same instance; with a different request it is
//     IdempotentParameterMismatch. A request is the launch config plus the
//     instant aws.Client.Launch stamps into spawn:launch-time and
//     spawn:ttl-deadline, which moves on every Launch call.
//   - reject names workers whose RunInstances EC2 rejects for the instance
//     profile, as Launch returns it once its retry gives up.
//   - needBudget: a Launch whose context leaves less than this fails as the SDK
//     does when the deadline ends the #572 retry. Zero disables it.
type poolEC2 struct {
	mu         sync.Mutex
	clock      int // Launch's launch-time stamp, one tick per call
	tokens     map[string]string
	insts      map[string]aws.InstanceInfo // instance id → instance
	next       int
	reject     map[string]bool
	needBudget time.Duration
}

func newPoolEC2() *poolEC2 {
	return &poolEC2{tokens: map[string]string{}, insts: map[string]aws.InstanceInfo{}, reject: map[string]bool{}}
}

func profileRejection() error {
	return &smithy.GenericAPIError{
		Code:    "InvalidParameterValue",
		Message: "Value (spawn-instance-pool-bcea2230f60c4c4b) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name",
	}
}

func (f *poolEC2) Launch(ctx context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	if f.needBudget > 0 {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < f.needBudget {
			return nil, fmt.Errorf("RunInstances retry stopped (%w) after 3 attempt(s) rejected with: %w", context.DeadlineExceeded, profileRejection())
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock++
	request := fmt.Sprintf("%s|%s|%s|%s|launch-time=%d", cfg.Name, cfg.InstanceType, cfg.IamInstanceProfile, cfg.UserData, f.clock)
	if cfg.ClientToken == "" {
		return nil, fmt.Errorf("worker %s launched with no ClientToken", cfg.Name)
	}
	if prev, seen := f.tokens[cfg.ClientToken]; seen {
		if prev != request {
			return nil, &smithy.GenericAPIError{Code: "IdempotentParameterMismatch", Message: "The client token you have provided is associated with a resource that is already deleted or with different parameters"}
		}
	}
	f.tokens[cfg.ClientToken] = request
	if f.reject[cfg.Name] {
		return nil, profileRejection()
	}
	f.next++
	id := fmt.Sprintf("i-%04d", f.next)
	f.insts[id] = aws.InstanceInfo{InstanceID: id, Name: cfg.Name, State: "running", Tags: cfg.Tags}
	return &aws.LaunchResult{InstanceID: id, Name: cfg.Name, State: "running"}, nil
}

func (f *poolEC2) Terminate(_ context.Context, _, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.insts[instanceID]
	in.State = "terminated"
	f.insts[instanceID] = in
	return nil
}

func (f *poolEC2) StartInstance(context.Context, string, string) error { return nil }

// ListInstances with no state filter, as aws.Client's: not terminated.
func (f *poolEC2) ListInstances(_ context.Context, _, _ string) ([]aws.InstanceInfo, error) {
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

// withPool sets the pool create flags a test's reconcile reads.
func withPool(t *testing.T, runID string, workers, minViable int) {
	t.Helper()
	saved := []any{poolRunID, poolWorkers, poolMinViable, poolInstanceType, poolSpot, poolTTL}
	poolRunID, poolWorkers, poolMinViable, poolInstanceType, poolSpot, poolTTL = runID, workers, minViable, "c8g.8xlarge", false, "4h"
	t.Cleanup(func() {
		poolRunID, poolWorkers, poolMinViable = saved[0].(string), saved[1].(int), saved[2].(int)
		poolInstanceType, poolSpot, poolTTL = saved[3].(string), saved[4].(bool), saved[5].(string)
	})
}

func reconcileTestPool(t *testing.T, ec2 *poolEC2, generation string) (string, error) {
	t.Helper()
	base := poolWorkerBaseConfig("us-west-2", "ami-0123", "spawn-instance-pool-bcea2230f60c4c4b", "IyEvYmluL3No")
	members, outcome, err := reconcilePoolWorkers(context.Background(), ec2, "us-west-2", base, generation)
	if err != nil {
		t.Fatalf("reconcilePoolWorkers: %v", err)
	}
	return poolProvisionResult(poolRunID, members, outcome, poolMinViableFor(poolWorkers))
}

// #1448 problem 1. Each worker's Launch retries the #572 rejection for
// aws.InstanceProfilePropagationRetryWindow, after its own calls before the
// first RunInstances (a few seconds in #1448). The pool's launch budget must
// hold all of it: cohort's default 15s cut the fourth worker off mid-retry.
// Both cohort shapes are covered, since --min-viable equal to --workers makes
// the pool launch as one unit.
func TestPoolLaunchBudgetHoldsTheProfileRetryWindow(t *testing.T) {
	for _, minViable := range []int{1, 4} {
		t.Run(fmt.Sprintf("min-viable %d", minViable), func(t *testing.T) {
			withPool(t, "oeb-kissat-m2", 4, minViable)
			ec2 := newPoolEC2()
			ec2.needBudget = aws.InstanceProfilePropagationRetryWindow() + 10*time.Second
			ready, err := reconcileTestPool(t, ec2, "q1")
			if err != nil {
				t.Fatalf("a worker's launch was cut off before its profile retry could finish: %v", err)
			}
			if !strings.Contains(ready, "ready: 4 of 4 workers") {
				t.Errorf("ready report = %q, want all 4 workers", ready)
			}
		})
	}
}

// #1448 problem 2. A worker that fails says why, in AWS's words, and so does
// every worker cancelled around it. "terminal/ at phase=launch-acked" told the
// user nothing, least of all that the error was transient.
func TestPoolFailureNamesTheAWSError(t *testing.T) {
	withPool(t, "oeb-kissat-m2", 4, 4)
	ec2 := newPoolEC2()
	ec2.reject["oeb-kissat-m2-worker-1"] = true
	_, err := reconcileTestPool(t, ec2, "q1")
	if err == nil {
		t.Fatal("a 4-of-4 pool with a rejected worker reported ready")
	}
	const reason = "terminal/InvalidParameterValue: Value (spawn-instance-pool-bcea2230f60c4c4b) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name"
	for i := 0; i < 4; i++ {
		var line string
		for _, l := range strings.Split(err.Error(), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), fmt.Sprintf("oeb-kissat-m2-worker-%d:", i)) {
				line = l
			}
		}
		if !strings.Contains(line, reason) {
			t.Errorf("worker-%d's line does not carry the AWS error %q:\n%s", i, reason, err)
		}
	}
}

// The reason survives a retry cut short by the launch deadline too: the code
// and message are the rejection's, with the deadline noted.
func TestPoolFailureNamesTheAWSErrorWhenTheDeadlineEndsTheRetry(t *testing.T) {
	withPool(t, "oeb-kissat-m2", 1, 1)
	ec2 := newPoolEC2()
	ec2.needBudget = 365 * 24 * time.Hour // no budget is enough
	_, err := reconcileTestPool(t, ec2, "q1")
	if err == nil {
		t.Fatal("expected the worker to fail")
	}
	for _, want := range []string{"terminal/InvalidParameterValue: ", "Invalid IAM Instance Profile name", context.DeadlineExceeded.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure does not contain %q:\n%s", want, err)
		}
	}
}

// #1448 problem 3. With --min-viable 1, a pool that came up with 1 of 4
// workers said "ready: requested 4 workers". It must say how many exist and
// name each missing worker with its reason.
func TestPoolReadySaysHowManyWorkersExist(t *testing.T) {
	withPool(t, "oeb-kissat-m1", 4, 1)
	ec2 := newPoolEC2()
	for _, i := range []int{0, 2, 3} {
		ec2.reject[fmt.Sprintf("oeb-kissat-m1-worker-%d", i)] = true
	}
	ready, err := reconcileTestPool(t, ec2, "q1")
	if err != nil {
		t.Fatalf("min viable 1 with one worker up should be ready: %v", err)
	}
	if !strings.Contains(ready, "ready: 1 of 4 workers") {
		t.Errorf("ready report does not say 1 of 4:\n%s", ready)
	}
	for _, i := range []int{0, 2, 3} {
		want := fmt.Sprintf("oeb-kissat-m1-worker-%d: terminal/InvalidParameterValue: ", i)
		if !strings.Contains(ready, want) {
			t.Errorf("ready report does not list %q with its reason:\n%s", want, ready)
		}
	}
	if strings.Contains(ready, "oeb-kissat-m1-worker-1:") {
		t.Errorf("ready report lists the worker that came up as missing:\n%s", ready)
	}
}

// #1448 problem 4. A failed `pool create` is retried with the same run id. The
// failed attempt deleted its queue, so the retry has a new queue incarnation
// and must launch every worker; with tokens fixed per run id and worker, EC2
// refused them all with IdempotentParameterMismatch, because Launch's
// timestamps had moved.
func TestPoolCreateRetriesUnderTheSameRunID(t *testing.T) {
	withPool(t, "oeb-kissat-m2", 4, 4)
	ec2 := newPoolEC2()
	ec2.reject["oeb-kissat-m2-worker-1"] = true
	if _, err := reconcileTestPool(t, ec2, "q1761853661"); err == nil {
		t.Fatal("the first attempt was meant to fail")
	}
	if live, _ := poolLiveWorkers(context.Background(), ec2, "us-west-2", poolRunID); len(live) != 0 {
		t.Fatalf("the failed attempt left live workers: %v", live)
	}

	ec2.reject = map[string]bool{} // the profile has propagated
	ready, err := reconcileTestPool(t, ec2, "q1761853790")
	if err != nil {
		t.Fatalf("retrying a failed create with the same run id: %v", err)
	}
	if !strings.Contains(ready, "ready: 4 of 4 workers") {
		t.Errorf("retry report = %q, want 4 of 4", ready)
	}
}

// The other half of problem 4: within one queue incarnation the tokens do not
// change, so a create while the pool is live sends the live workers' tokens
// and cannot add a second set. poolLiveWorkers refuses it first.
func TestPoolCreateCannotDuplicateALivePool(t *testing.T) {
	withPool(t, "oeb-kissat-p1", 2, 2)
	ec2 := newPoolEC2()
	if _, err := reconcileTestPool(t, ec2, "q1761853661"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	live, err := poolLiveWorkers(context.Background(), ec2, "us-west-2", poolRunID)
	if err != nil || len(live) != 2 {
		t.Fatalf("poolLiveWorkers = %v, %v; want the 2 live workers", live, err)
	}
	// The guard bypassed, the same queue gives the same tokens.
	if _, err := reconcileTestPool(t, ec2, "q1761853661"); err == nil {
		t.Fatal("a second create on the live pool's queue launched workers")
	}
	if got, _ := ec2.ListInstances(context.Background(), "us-west-2", ""); len(got) != 2 {
		t.Errorf("%d live instances after a second create on a live pool, want 2", len(got))
	}
	// And the tokens are a function of the queue incarnation.
	rung := cohort.Rung{InstanceType: "c8g.8xlarge", CapacityModel: cohort.CapacityOnDemand}
	a, _ := poolWorkerIntents(poolRunID, "q1", 2, rung)
	b, _ := poolWorkerIntents(poolRunID, "q1", 2, rung)
	c, _ := poolWorkerIntents(poolRunID, "q2", 2, rung)
	for i := range a {
		if a[i].IdempotencyToken != b[i].IdempotencyToken {
			t.Errorf("worker %d: two token derivations for one queue differ", i)
		}
		if a[i].IdempotencyToken == c[i].IdempotencyToken {
			t.Errorf("worker %d: two queue incarnations share a token", i)
		}
	}
}

type incarnationStub struct {
	inc string
	err error
}

func (s incarnationStub) Incarnation(context.Context) (string, error) { return s.inc, s.err }

// The generation is the queue's incarnation; without sqs:GetQueueAttributes it
// is one per attempt, with a warning, rather than a failed create.
func TestPoolWorkerGenerationFollowsTheQueue(t *testing.T) {
	var warn strings.Builder
	if g := poolWorkerGeneration(context.Background(), incarnationStub{inc: "1761853661"}, &warn); g != "q1761853661" || warn.Len() != 0 {
		t.Errorf("generation = %q (warning %q), want q1761853661 and no warning", g, warn.String())
	}
	denied := incarnationStub{err: fmt.Errorf("AccessDenied: sqs:GetQueueAttributes")}
	a := poolWorkerGeneration(context.Background(), denied, &warn)
	b := poolWorkerGeneration(context.Background(), denied, &warn)
	if a == "" || a == b || !strings.Contains(warn.String(), "AccessDenied") {
		t.Errorf("without the queue's creation time: generations %q, %q (want two distinct), warning %q", a, b, warn.String())
	}
}
