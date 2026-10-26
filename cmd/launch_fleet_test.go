package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/mpicohort"
	"github.com/spore-host/spawn/pkg/progress"
)

// Gates for #1305 (multi-instance launches verify spored and clean up after
// themselves, as a single launch does since #1298) and #1251 (the rolling queue
// polls every region in the sweep). Every assertion counts Terminate calls
// through the guard's seam; none measures time.

// countingTerminator counts Terminate calls per instance.
type countingTerminator struct {
	mu    sync.Mutex
	calls map[string]int
	fail  map[string]bool
}

func newCountingTerminator() *countingTerminator {
	return &countingTerminator{calls: map[string]int{}, fail: map[string]bool{}}
}

func (c *countingTerminator) Terminate(_ context.Context, _, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[id]++
	if c.fail[id] {
		return errors.New("UnauthorizedOperation")
	}
	return nil
}

func (c *countingTerminator) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

func (c *countingTerminator) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

// sporedDownOn is a verifier under which spored never comes up on the given
// instances and is confirmed on every other.
func sporedDownOn(ids ...string) fleetVerifyFunc {
	down := map[string]bool{}
	for _, id := range ids {
		down[id] = true
	}
	return func(_ context.Context, m fleetMember, _ time.Duration) error {
		if down[m.instanceID] {
			return errors.New("no spawn:last-heartbeat tag")
		}
		return nil
	}
}

func sporedUp() fleetVerifyFunc { return sporedDownOn() }

func members(ids ...string) []fleetMember {
	out := make([]fleetMember, len(ids))
	for i, id := range ids {
		out[i] = fleetMember{label: "m" + id, instanceID: id, region: "us-east-1"}
	}
	return out
}

func wantEachTerminatedOnce(t *testing.T, term *countingTerminator, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if n := term.count(id); n != 1 {
			t.Errorf("%s terminated %d times, want exactly 1", id, n)
		}
	}
	if term.total() != len(ids) {
		t.Errorf("%d Terminate calls in all, want %d (one per launched member): %v", term.total(), len(ids), term.calls)
	}
}

// --- the guard itself ---

func TestFleetGuardFinishTerminatesEveryLaunchedMemberOnce(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("sweep", io.Discard, term, sporedUp(), false)
	for _, m := range members("i-1", "i-2", "i-3") {
		g.launched(m)
	}
	g.launched(members("i-2")[0]) // recorded twice (recorder + list): still one member

	err := g.finish(context.Background(), errors.New("list instances: throttled"))
	if err == nil || !strings.Contains(err.Error(), "terminated them") {
		t.Fatalf("finish = %v, want the failure, saying the members were terminated", err)
	}
	// A second finish (or a later return through it) must not terminate again.
	_ = g.finish(context.Background(), errors.New("again"))
	wantEachTerminatedOnce(t, term, "i-1", "i-2", "i-3")
}

func TestFleetGuardFinishSkipsWhatTheDrainTerminated(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("job array", io.Discard, term, sporedUp(), false)
	for _, m := range members("i-1", "i-2") {
		g.launched(m)
	}
	g.markGone("i-1")
	_ = g.finish(context.Background(), errors.New("cohort failed"))
	wantEachTerminatedOnce(t, term, "i-2")
}

func TestFleetGuardKeepOnErrorKeepsMembersWithAWarning(t *testing.T) {
	term := newCountingTerminator()
	var out bytes.Buffer
	g := newFleetGuard("burst", &out, term, sporedUp(), true)
	for _, m := range members("i-1", "i-2") {
		g.launched(m)
	}
	err := g.finish(context.Background(), errors.New("boom"))
	if err == nil || !strings.Contains(err.Error(), "kept them (--keep-on-error)") {
		t.Fatalf("finish = %v, want the failure, saying the members were kept", err)
	}
	if term.total() != 0 {
		t.Fatalf("--keep-on-error terminated %v", term.calls)
	}
	for _, want := range []string{"--keep-on-error", "i-1", "i-2", "NO TTL"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("warning does not mention %q:\n%s", want, out.String())
		}
	}
}

func TestFleetGuardPassesSuccessAndPartialOutcomesThrough(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("sweep", io.Discard, term, sporedUp(), false)
	g.launched(members("i-1")[0])
	if err := g.finish(context.Background(), nil); err != nil {
		t.Fatalf("finish(nil) = %v", err)
	}
	p := partial(errors.New("1/2 parameter sets failed"))
	if err := g.finish(context.Background(), p); err != p {
		t.Fatalf("finish(partial) = %v, want it unchanged", err)
	}
	if term.total() != 0 {
		t.Fatalf("a success or a partial outcome terminated %v", term.calls)
	}
}

func TestFleetGuardReportsAMemberItCouldNotTerminate(t *testing.T) {
	term := newCountingTerminator()
	term.fail["i-2"] = true
	g := newFleetGuard("sweep", io.Discard, term, sporedUp(), false)
	for _, m := range members("i-1", "i-2") {
		g.launched(m)
	}
	err := g.finish(context.Background(), errors.New("boom"))
	if err == nil || !strings.Contains(err.Error(), "STILL RUNNING") || !strings.Contains(err.Error(), "i-2") {
		t.Fatalf("finish = %v, want it to name i-2 as still running", err)
	}
}

// --- verification ---

func TestFleetVerifyTerminatesOnlyTheMemberWhoseSporedNeverCameUp(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("job array", io.Discard, term, sporedDownOn("i-2"), false)
	ms := members("i-1", "i-2", "i-3")
	for _, m := range ms {
		g.launched(m)
	}
	got := g.verifyAll(context.Background(), ms)
	if ids := memberIDs(got); ids != "i-1,i-3" {
		t.Fatalf("verified %s, want i-1,i-3", ids)
	}
	wantEachTerminatedOnce(t, term, "i-2")
	s := g.summary()
	for _, want := range []string{"verified on 2 of 3", "1 terminated", "i-2"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q lacks %q", s, want)
		}
	}
}

func TestFleetVerifyKeepOnErrorKeepsTheUnverifiedMember(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("job array", io.Discard, term, sporedDownOn("i-2"), true)
	ms := members("i-1", "i-2")
	got := g.verifyAll(context.Background(), ms)
	if ids := memberIDs(got); ids != "i-1" {
		t.Fatalf("verified %s, want i-1", ids)
	}
	if term.total() != 0 {
		t.Fatalf("--keep-on-error terminated %v", term.calls)
	}
	if s := g.summary(); !strings.Contains(s, "1 kept unverified") {
		t.Errorf("summary %q does not report the kept member", s)
	}
}

// The members share one deadline: a member that waited for a slot gets what
// is left of the budget, not a fresh five minutes, so a fleet takes about as
// long as one instance, never N times as long. The floor keeps one real poll.
func TestFleetVerifySharesOneDeadline(t *testing.T) {
	var got []time.Duration
	g := newFleetGuard("sweep", io.Discard, newCountingTerminator(), func(_ context.Context, _ fleetMember, timeout time.Duration) error {
		got = append(got, timeout)
		return nil
	}, false)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	m := members("i-1")[0]
	budget := sporedVerifyTimeout(sporedCheckLinux)

	_ = g.verifyOne(context.Background(), m, now)
	_ = g.verifyOne(context.Background(), m, now.Add(-budget+time.Minute))
	_ = g.verifyOne(context.Background(), m, now.Add(-2*budget))
	want := []time.Duration{budget, time.Minute, fleetVerifyFloor}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("timeouts = %v, want %v", got, want)
	}
}

func TestFleetVerifyUsesTheWindowsBudgetForWindowsMembers(t *testing.T) {
	var got time.Duration
	g := newFleetGuard("sweep", io.Discard, newCountingTerminator(), func(_ context.Context, _ fleetMember, timeout time.Duration) error {
		got = timeout
		return nil
	}, false)
	now := time.Now()
	g.now = func() time.Time { return now }
	_ = g.verifyOne(context.Background(), fleetMember{instanceID: "i-w", targetOS: "windows"}, now)
	if got != sporedVerifyTimeout(sporedCheckWindows) {
		t.Fatalf("windows member got %v", got)
	}
}

func memberIDs(ms []fleetMember) string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.instanceID
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// --- job array ---

// fakeArrayAPI is arrayFleetAPI over a fixed instance list.
type fakeArrayAPI struct {
	*countingTerminator
	insts   []aws.InstanceInfo
	listErr error
	lists   int
}

func (f *fakeArrayAPI) ListInstances(context.Context, string, string) ([]aws.InstanceInfo, error) {
	f.lists++
	return f.insts, f.listErr
}

func (f *fakeArrayAPI) GetInstancePublicIP(context.Context, string, string) (string, error) {
	return "", nil
}

// arrayCase launches members w-0..w-(n-1) as i-0..i-(n-1), recording each in
// the guard as the Actuator's recorder does, and returns the fake EC2 view.
func arrayCase(n int) ([]cohort.EntityID, []aws.InstanceInfo) {
	ids := make([]cohort.EntityID, n)
	insts := make([]aws.InstanceInfo, n)
	for i := 0; i < n; i++ {
		ids[i] = cohort.EntityID(fmt.Sprintf("w-%d", i))
		insts[i] = aws.InstanceInfo{InstanceID: fmt.Sprintf("i-%d", i), Name: string(ids[i]), JobArrayID: "ja-1", State: "running"}
	}
	return ids, insts
}

func reconcileThatLaunches(g *fleetGuard, insts []aws.InstanceInfo, outcome cohort.Outcome, err error) func(context.Context) (cohort.Outcome, error) {
	return func(context.Context) (cohort.Outcome, error) {
		for _, in := range insts {
			g.launched(fleetMember{label: in.Name, instanceID: in.InstanceID, region: "us-east-1"})
		}
		return outcome, err
	}
}

func arrayRunFor(ids []cohort.EntityID, mpi bool, minViable int) arrayRun {
	return arrayRun{
		prog: progress.NewQuietProgress(), auditLog: audit.NewLogger(io.Discard, "u", "c"),
		kind: "job array", region: "us-east-1", jobArrayID: "ja-1",
		memberIDs: ids, mpi: mpi, minViable: minViable,
	}
}

func allIDs(insts []aws.InstanceInfo) []string {
	out := make([]string, len(insts))
	for i, in := range insts {
		out[i] = in.InstanceID
	}
	return out
}

// The failure in the issue: the cohort came up, then ListInstances failed and
// the launch exited non-zero with every member running.
func TestJobArrayListInstancesFailureTerminatesEveryMember(t *testing.T) {
	ids, insts := arrayCase(3)
	api := &fakeArrayAPI{countingTerminator: newCountingTerminator(), listErr: errors.New("RequestLimitExceeded")}
	g := newFleetGuard("job array", io.Discard, api, sporedUp(), false)
	_, err := runArrayCohort(context.Background(), api, g, reconcileThatLaunches(g, insts, cohort.Outcome{Ready: true}, nil), arrayRunFor(ids, false, 1))
	if err == nil {
		t.Fatal("want the ListInstances failure")
	}
	wantEachTerminatedOnce(t, api.countingTerminator, allIDs(insts)...)
}

func TestJobArrayListInstancesFailureKeepOnErrorKeeps(t *testing.T) {
	ids, insts := arrayCase(3)
	api := &fakeArrayAPI{countingTerminator: newCountingTerminator(), listErr: errors.New("RequestLimitExceeded")}
	g := newFleetGuard("job array", io.Discard, api, sporedUp(), true)
	_, err := runArrayCohort(context.Background(), api, g, reconcileThatLaunches(g, insts, cohort.Outcome{Ready: true}, nil), arrayRunFor(ids, false, 1))
	if err == nil || !strings.Contains(err.Error(), "--keep-on-error") {
		t.Fatalf("err = %v, want the failure saying the members were kept", err)
	}
	if api.total() != 0 {
		t.Fatalf("--keep-on-error terminated %v", api.calls)
	}
}

func TestJobArrayReconcileErrorTerminatesWhatItLaunched(t *testing.T) {
	ids, insts := arrayCase(2)
	api := &fakeArrayAPI{countingTerminator: newCountingTerminator(), insts: insts}
	g := newFleetGuard("job array", io.Discard, api, sporedUp(), false)
	_, err := runArrayCohort(context.Background(), api, g, reconcileThatLaunches(g, insts, cohort.Outcome{}, errors.New("context deadline exceeded")), arrayRunFor(ids, false, 1))
	if err == nil {
		t.Fatal("want the reconcile error")
	}
	wantEachTerminatedOnce(t, api.countingTerminator, allIDs(insts)...)
}

// A failed cohort is drained by tag; a member the tag scan missed is still
// terminated, and none is terminated twice.
func TestJobArrayFailedCohortTerminatesEachMemberOnce(t *testing.T) {
	ids, insts := arrayCase(3)
	// The scan sees only i-0 and i-1 (eventual consistency); i-2 was launched.
	api := &fakeArrayAPI{countingTerminator: newCountingTerminator(), insts: insts[:2]}
	g := newFleetGuard("job array", io.Discard, api, sporedUp(), false)
	_, err := runArrayCohort(context.Background(), api, g, reconcileThatLaunches(g, insts, cohort.Outcome{Ready: false}, nil), arrayRunFor(ids, true, 3))
	if err == nil {
		t.Fatal("want the cohort failure")
	}
	wantEachTerminatedOnce(t, api.countingTerminator, allIDs(insts)...)
}

func TestJobArrayTerminatesOnlyTheUnverifiedMemberAndKeepsTheRest(t *testing.T) {
	ids, insts := arrayCase(3)
	api := &fakeArrayAPI{countingTerminator: newCountingTerminator(), insts: insts}
	g := newFleetGuard("job array", io.Discard, api, sporedDownOn("i-1"), false)
	got, err := runArrayCohort(context.Background(), api, g, reconcileThatLaunches(g, insts, cohort.Outcome{Ready: true}, nil), arrayRunFor(ids, false, 1))
	if err != nil {
		t.Fatalf("a plain array above --min-viable must succeed: %v", err)
	}
	if len(got) != 2 || got[0].InstanceID != "i-0" || got[1].InstanceID != "i-2" {
		t.Fatalf("kept %v, want i-0 and i-2", got)
	}
	wantEachTerminatedOnce(t, api.countingTerminator, "i-1")
}

func TestJobArrayBelowMinViableAfterVerificationTerminatesAll(t *testing.T) {
	ids, insts := arrayCase(3)
	api := &fakeArrayAPI{countingTerminator: newCountingTerminator(), insts: insts}
	g := newFleetGuard("job array", io.Discard, api, sporedDownOn("i-1"), false)
	_, err := runArrayCohort(context.Background(), api, g, reconcileThatLaunches(g, insts, cohort.Outcome{Ready: true}, nil), arrayRunFor(ids, false, 3))
	if err == nil || !strings.Contains(err.Error(), "verified on 2 of 3") {
		t.Fatalf("err = %v, want a failure: 2 verified, 3 needed", err)
	}
	wantEachTerminatedOnce(t, api.countingTerminator, allIDs(insts)...)
}

func TestMPIClusterWithAnUnverifiedRankTerminatesAll(t *testing.T) {
	ids, insts := arrayCase(2)
	api := &fakeArrayAPI{countingTerminator: newCountingTerminator(), insts: insts}
	g := newFleetGuard("MPI cluster", io.Discard, api, sporedDownOn("i-0"), false)
	_, err := runArrayCohort(context.Background(), api, g, reconcileThatLaunches(g, insts, cohort.Outcome{Ready: true}, nil), arrayRunFor(ids, true, 2))
	if err == nil {
		t.Fatal("an MPI cluster missing a rank must fail")
	}
	wantEachTerminatedOnce(t, api.countingTerminator, allIDs(insts)...)
}

// launchOnlyAPI is an mpicohort.LaunchAPI that can only launch.
type launchOnlyAPI struct {
	mpicohort.LaunchAPI
	n int
}

func (l *launchOnlyAPI) Launch(_ context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	l.n++
	return &aws.LaunchResult{InstanceID: fmt.Sprintf("i-%d", l.n), Name: cfg.Name}, nil
}

// The Actuator the job array uses records every launch in the guard, which is
// what lets a later failure terminate exactly what was created.
func TestJobArrayActuatorRecordsEveryLaunchInTheGuard(t *testing.T) {
	g := newFleetGuard("job array", io.Discard, newCountingTerminator(), sporedUp(), false)
	base := &aws.LaunchConfig{Region: "us-east-1"}
	act := newArrayActuator(&launchOnlyAPI{}, g, base, nil)
	for _, name := range []string{"w-0", "w-1"} {
		if _, err := act.Client.Launch(context.Background(), aws.LaunchConfig{Name: name, Region: "us-east-1"}); err != nil {
			t.Fatal(err)
		}
	}
	if ids := memberIDs(g.live()); ids != "i-1,i-2" {
		t.Fatalf("guard holds %s, want i-1,i-2", ids)
	}
}

// --- sweep ---

// fakeFleetSweep is a sweepLauncher whose instances live in the region they
// were launched into, and finish after finishAfter polls of that region.
type fakeFleetSweep struct {
	*countingTerminator
	mu          sync.Mutex
	n           int
	failLaunch  map[string]bool // config name → RunInstances fails
	region      map[string]string
	polls       map[string]int // per instance: polls of its region seen
	finishAfter int
	running     map[string]bool
	maxRunning  int
	regionsSeen map[string]int
}

func newFakeFleetSweep(finishAfter int) *fakeFleetSweep {
	return &fakeFleetSweep{
		countingTerminator: newCountingTerminator(),
		failLaunch:         map[string]bool{}, region: map[string]string{}, polls: map[string]int{},
		running: map[string]bool{}, regionsSeen: map[string]int{}, finishAfter: finishAfter,
	}
}

func (f *fakeFleetSweep) Launch(_ context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failLaunch[cfg.Name] {
		return nil, errors.New("VcpuLimitExceeded")
	}
	f.n++
	id := fmt.Sprintf("i-%s", cfg.Name)
	f.region[id] = cfg.Region
	f.running[id] = true
	if len(f.running) > f.maxRunning {
		f.maxRunning = len(f.running)
	}
	return &aws.LaunchResult{InstanceID: id, Name: cfg.Name, Region: cfg.Region, State: "pending"}, nil
}

func (f *fakeFleetSweep) ListInstances(_ context.Context, region, _ string) ([]aws.InstanceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regionsSeen[region]++
	var out []aws.InstanceInfo
	for id, r := range f.region {
		if r != region {
			continue
		}
		f.polls[id]++
		st := "running"
		if f.polls[id] > f.finishAfter {
			st = "terminated"
			delete(f.running, id)
		}
		out = append(out, aws.InstanceInfo{InstanceID: id, State: st})
	}
	return out, nil
}

func (f *fakeFleetSweep) Terminate(ctx context.Context, region, id string) error {
	f.mu.Lock()
	delete(f.running, id)
	f.mu.Unlock()
	return f.countingTerminator.Terminate(ctx, region, id)
}

func sweepConfigs(regions ...string) []*aws.LaunchConfig {
	out := make([]*aws.LaunchConfig, len(regions))
	for i, r := range regions {
		out[i] = &aws.LaunchConfig{Name: fmt.Sprintf("s-%d", i), Region: r}
	}
	return out
}

// #1251: a two-region sweep at max-concurrent 1. The member in the second
// region must be watched in its own region; polling only the first made it
// look finished at once, so the queue launched past the limit.
func TestRollingQueuePollsEveryRegionInTheSweep(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)
	f := newFakeFleetSweep(2)
	cfgs := sweepConfigs("us-east-1", "eu-west-1", "us-east-1")
	state := &SweepState{SweepID: "sweep-1251", TotalParams: len(cfgs), LaunchDelay: "0s"}
	g := newFleetGuard("sweep", io.Discard, f, sporedUp(), false)
	_, failures, n, err := launchWithRollingQueue(context.Background(), f, g, state, cfgs, []int{0, 1, 2}, 1, "0s")
	if err != nil || n != 3 || len(failures) != 0 {
		t.Fatalf("n=%d failures=%v err=%v", n, failures, err)
	}
	if f.regionsSeen["eu-west-1"] == 0 {
		t.Errorf("never polled eu-west-1; polls by region: %v", f.regionsSeen)
	}
	if f.maxRunning > 1 {
		t.Errorf("%d instances ran at once under max-concurrent 1: the other region's member was taken for finished", f.maxRunning)
	}
}

func TestRollingQueueTerminatesAndFailsAnUnverifiedMember(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)
	f := newFakeFleetSweep(1)
	cfgs := sweepConfigs("us-east-1", "us-east-1", "us-east-1")
	state := &SweepState{SweepID: "sweep-1305", TotalParams: len(cfgs), LaunchDelay: "0s"}
	g := newFleetGuard("sweep", io.Discard, f, sporedDownOn("i-s-1"), false)
	launched, failures, n, err := launchWithRollingQueue(context.Background(), f, g, state, cfgs, []int{0, 1, 2}, 1, "0s")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(failures) != 1 || !strings.Contains(failures[0], "Parameter set 1") {
		t.Fatalf("n=%d failures=%v, want 2 kept and parameter set 1 failed", n, failures)
	}
	if launched[1] != nil {
		t.Error("the unverified member is still reported as launched")
	}
	wantEachTerminatedOnce(t, f.countingTerminator, "i-s-1")

	saved, err := loadSweepState("sweep-1305")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Failed != 1 {
		t.Errorf("saved Failed = %d, want 1", saved.Failed)
	}
	// It was terminated, which reconcile would otherwise read as "completed".
	_, completed, failed, _ := reconcileSweepState(saved, map[string]string{"i-s-1": "terminated"})
	if fmt.Sprint(failed) != "[1]" || strings.Contains(fmt.Sprint(completed), "1") {
		t.Errorf("reconcile: completed=%v failed=%v, want parameter set 1 failed", completed, failed)
	}
}

func TestSweepAllAtOnceTerminatesOnlyTheUnverifiedMember(t *testing.T) {
	setTestHome(t, t.TempDir())
	f := newFakeFleetSweep(0)
	g := newFleetGuard("sweep", io.Discard, f, sporedDownOn("i-s-0"), false)
	err := runSweepLaunch(context.Background(), f, g, sweepRun{
		sweepID: "s", name: "s", totalParams: 3, configs: sweepConfigs("us-east-1", "us-east-1", "us-east-1"),
	})
	var p *fleetPartialError
	if err == nil || !errors.As(err, &p) || !strings.Contains(err.Error(), "1/3 parameter sets failed") {
		t.Fatalf("err = %v, want a partial outcome naming 1/3 failed", err)
	}
	wantEachTerminatedOnce(t, f.countingTerminator, "i-s-0")
}

func TestSweepKeepOnErrorKeepsTheUnverifiedMember(t *testing.T) {
	setTestHome(t, t.TempDir())
	f := newFakeFleetSweep(0)
	g := newFleetGuard("sweep", io.Discard, f, sporedDownOn("i-s-0"), true)
	_ = runSweepLaunch(context.Background(), f, g, sweepRun{
		sweepID: "s", name: "s", totalParams: 2, configs: sweepConfigs("us-east-1", "us-east-1"),
	})
	if f.total() != 0 {
		t.Fatalf("--keep-on-error terminated %v", f.calls)
	}
}

// A row RunInstances refused is that row's failure; its verified siblings stay.
func TestSweepLaunchFailureKeepsVerifiedSiblings(t *testing.T) {
	setTestHome(t, t.TempDir())
	f := newFakeFleetSweep(0)
	f.failLaunch["s-1"] = true
	g := newFleetGuard("sweep", io.Discard, f, sporedUp(), false)
	err := runSweepLaunch(context.Background(), f, g, sweepRun{
		sweepID: "s", name: "s", totalParams: 2, configs: sweepConfigs("us-east-1", "us-east-1"),
	})
	if err == nil {
		t.Fatal("want the failed row reported")
	}
	if f.total() != 0 {
		t.Fatalf("a sibling's launch failure terminated verified members: %v", f.calls)
	}
}

// --- burst ---

func TestBurstStoppedPartWayTerminatesWhatItLaunched(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("burst", io.Discard, term, sporedUp(), false)
	ids, err := runBurstLaunch(context.Background(), &fakeBurstLauncher{failAt: 3}, g, burstTestParams())
	if err == nil {
		t.Fatal("want the failed third launch")
	}
	if len(ids) != 0 {
		t.Errorf("reported %v as kept after terminating them", ids)
	}
	wantEachTerminatedOnce(t, term, "i-burst1", "i-burst2")
}

func TestBurstStoppedPartWayKeepOnErrorKeeps(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("burst", io.Discard, term, sporedUp(), true)
	if _, err := runBurstLaunch(context.Background(), &fakeBurstLauncher{failAt: 3}, g, burstTestParams()); err == nil {
		t.Fatal("want the failed third launch")
	}
	if term.total() != 0 {
		t.Fatalf("--keep-on-error terminated %v", term.calls)
	}
}

func TestBurstTerminatesOnlyTheUnverifiedMember(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("burst", io.Discard, term, sporedDownOn("i-burst2"), false)
	ids, err := runBurstLaunch(context.Background(), &fakeBurstLauncher{}, g, burstTestParams())
	if err == nil || !strings.Contains(err.Error(), "1 of 3") {
		t.Fatalf("err = %v, want it to report 1 of 3 unverified", err)
	}
	if strings.Join(ids, ",") != "i-burst1,i-burst3" {
		t.Fatalf("kept %v, want i-burst1,i-burst3", ids)
	}
	wantEachTerminatedOnce(t, term, "i-burst2")
}

// terminate is at most once per member over the guard's life, however it is
// reached (a failed verification, then a failed launch).
func TestFleetGuardTerminatesAMemberAtMostOnce(t *testing.T) {
	term := newCountingTerminator()
	g := newFleetGuard("sweep", io.Discard, term, sporedUp(), false)
	m := members("i-1")[0]
	_ = g.terminate(context.Background(), m)
	_ = g.terminate(context.Background(), m)
	wantEachTerminatedOnce(t, term, "i-1")
}
