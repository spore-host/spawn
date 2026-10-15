package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// fakeKernel stands in for CLOCK_MONOTONIC and CLOCK_BOOTTIME. run advances
// both; hibernate advances only CLOCK_BOOTTIME, which is what a suspend does.
type fakeKernel struct{ mono, boot time.Duration }

func (k *fakeKernel) run(d time.Duration)       { k.mono += d; k.boot += d }
func (k *fakeKernel) hibernate(d time.Duration) { k.boot += d }
func (k *fakeKernel) now() kernelClocks         { return kernelClocks{Monotonic: k.mono, Boottime: k.boot} }

// install makes k the agent's kernel for the rest of the test.
func (k *fakeKernel) install(t *testing.T, bootID string) {
	t.Helper()
	restore := SetKernelClocksForTest(bootID, func() (time.Duration, time.Duration, bool) { return k.mono, k.boot, true })
	t.Cleanup(restore)
}

const (
	hibInstance = "i-0abc945"
	hibBoot     = "boot-945"
	hibernation = 8 * time.Hour
)

// withinSecond reports whether got is want, to the second.
func withinSecond(got, want int64) bool { return got >= want-1 && got <= want+1 }

// The daemon counted 10 minutes of this boot, the instance hibernated for 8
// hours, it resumed — same boot, same /var/lib checkpoint — and spored then
// restarted (an upgrade, a crash). The restarted daemon must resume the count
// at 10 minutes, not at 8h10m: EC2 does not bill compute while hibernated, and
// this total is what --cost-limit is enforced against (#945).
func TestResolveComputeStart_RestartAfterAHibernationDoesNotCountIt(t *testing.T) {
	k := &fakeKernel{}
	k.install(t, hibBoot)
	now := time.Now()
	bootedAt := now.Add(-(10*time.Minute + hibernation))

	// The first daemon, counting this boot from the kernel's boot.
	first := &Agent{
		identity: &provider.Identity{InstanceID: hibInstance}, bootID: hibBoot,
		computeSecondsBase: 70, computeStart: bootedAt, startTime: bootedAt.Add(30 * time.Second),
		kernelOrigin: kernelClocks{}, kernelOriginKnown: true,
	}
	k.run(10 * time.Minute)
	path := filepath.Join(t.TempDir(), "compute-checkpoint.json")
	if err := WriteComputeCheckpoint(path, first.computeCheckpoint()); err != nil {
		t.Fatal(err)
	}
	k.hibernate(hibernation)
	cp, err := ReadComputeCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}

	kernelNow, _ := readKernelClocks()
	start := resolveComputeStart(computeStartInputs{
		InstanceID: hibInstance, BootID: hibBoot, BootTime: bootedAt, SporedAt: now,
		TagSeconds: 70 + 600, Checkpoint: &cp, Kernel: kernelNow, KernelOK: true,
	})
	restarted := &Agent{
		computeSecondsBase: start.Base, computeStart: start.From, startTime: now,
		kernelOrigin: start.Origin, kernelOriginKnown: start.OriginKnown,
	}
	if got := restarted.TotalComputeSeconds(); !withinSecond(got, 70+600) {
		t.Errorf("total after a hibernation and a restart = %ds, want %ds: the hibernation was counted as compute (%s)",
			got, 70+600, start.Source)
	}
	if got := restarted.HibernatedTime(); got != hibernation {
		t.Errorf("hibernated = %v, want %v", got, hibernation)
	}
}

// A same-boot checkpoint from a spored before #945 has only wall times, which
// cannot tell a hibernation from running. Its recorded total is carried and the
// count resumes from the restart, so the hibernation is not counted.
func TestResolveComputeStart_PreKernelClockCheckpointIsNotMeasuredOnTheWallClock(t *testing.T) {
	k := &fakeKernel{}
	k.install(t, hibBoot)
	now := time.Now()
	bootedAt := now.Add(-(10*time.Minute + hibernation))
	cp := &ComputeCheckpoint{
		InstanceID: hibInstance, BootID: hibBoot, BaseSeconds: 70, CountFrom: bootedAt,
		TotalSeconds: 70 + 600, WrittenAt: bootedAt.Add(10 * time.Minute),
	}
	k.run(10 * time.Minute)
	k.hibernate(hibernation)

	start := resolveComputeStart(computeStartInputs{
		InstanceID: hibInstance, BootID: hibBoot, BootTime: bootedAt, SporedAt: now,
		Checkpoint: cp, Kernel: k.now(), KernelOK: true,
	})
	a := &Agent{
		computeSecondsBase: start.Base, computeStart: start.From, startTime: now,
		kernelOrigin: start.Origin, kernelOriginKnown: start.OriginKnown,
	}
	if got := a.TotalComputeSeconds(); !withinSecond(got, 70+600) {
		t.Errorf("total = %ds, want the checkpoint's %ds (%s)", got, 70+600, start.Source)
	}
}

// Counting "from the kernel's boot" must mean the kernel's running time, not
// its wall-clock age: when the instance hibernated before spored started (it
// was installed late in the boot), the boot's age includes the hibernation.
func TestResolveComputeStart_FromBootExcludesAnEarlierHibernation(t *testing.T) {
	k := &fakeKernel{}
	k.install(t, hibBoot)
	k.run(time.Minute)
	k.hibernate(hibernation)
	now := time.Now()
	bootedAt := now.Add(-k.boot) // /proc/uptime is CLOCK_BOOTTIME

	start := resolveComputeStart(computeStartInputs{
		InstanceID: hibInstance, BootID: hibBoot, BootTime: bootedAt, SporedAt: now,
		Kernel: k.now(), KernelOK: true,
	})
	if !start.From.Equal(bootedAt) {
		t.Errorf("From = %v, want the kernel boot %v (status measures boot time from it)", start.From, bootedAt)
	}
	a := &Agent{computeStart: start.From, startTime: now, kernelOrigin: start.Origin, kernelOriginKnown: start.OriginKnown}
	if got := a.TotalComputeSeconds(); !withinSecond(got, 60) {
		t.Errorf("first-boot compute = %ds, want the 60s the kernel ran, not its %v age", got, k.boot)
	}
}

// The daemon publishes its clock, the instance hibernates, and `spored status`
// — a fresh process, reading the clock back from /run after the resume — must
// count the daemon's running time, not the hibernation (#945).
func TestComputeClock_StatusAfterAHibernationCountsOnlyRunningTime(t *testing.T) {
	k := &fakeKernel{}
	k.install(t, "i-0123456789abcdef0-boot")
	now := time.Now()
	daemon := &Agent{
		identity: &provider.Identity{InstanceID: "i-0123456789abcdef0"}, bootID: "i-0123456789abcdef0-boot",
		computeSecondsBase: 600, computeStart: now.Add(-(90*time.Second + hibernation)),
		kernelOrigin: k.now(), kernelOriginKnown: true,
	}
	path := filepath.Join(t.TempDir(), "clock.json")
	if err := WriteComputeClock(path, daemon.ComputeClock()); err != nil {
		t.Fatal(err)
	}
	k.run(30 * time.Second)
	k.hibernate(hibernation)
	k.run(60 * time.Second)

	c, err := ReadComputeClock(path)
	if err != nil {
		t.Fatal(err)
	}
	status := clockQueryAgent(t, 0)
	if err := status.AdoptComputeClock(c, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("AdoptComputeClock: %v", err)
	}
	if got := status.TotalComputeSeconds(); !withinSecond(got, 600+90) {
		t.Errorf("status compute = %ds, want 600 + the 90s the daemon ran; the hibernation was counted", got)
	}
	if got := status.HibernatedTime(); got != hibernation {
		t.Errorf("status hibernated = %v, want %v", got, hibernation)
	}
}

// Kernel clock readings mean nothing outside their boot, and a reading later
// than now is not one from earlier in this boot.
func TestComputeClock_RefusesKernelReadingsItCannotMeasureFrom(t *testing.T) {
	k := &fakeKernel{mono: time.Minute, boot: time.Minute}
	k.install(t, "this-boot")
	now := time.Now()
	cases := map[string]ComputeClock{
		"another boot": {BootID: "other-boot"},
		"no boot id":   {},
		"from later":   {BootID: "this-boot", StartMonotonicNS: int64(time.Hour), StartBoottimeNS: int64(time.Hour)},
		"corrupt":      {BootID: "this-boot", StartMonotonicNS: int64(30 * time.Second), StartBoottimeNS: int64(time.Second)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			c.InstanceID, c.StartedAt, c.KernelClock = "i-0123456789abcdef0", now.Add(-30*time.Second), true
			a := clockQueryAgent(t, 42)
			if err := a.AdoptComputeClock(c, now.Add(-time.Hour)); err == nil {
				t.Fatal("adopted kernel readings it cannot measure from")
			}
			if a.ComputeClockLive() || a.TotalComputeSeconds() != 42 {
				t.Errorf("a refused clock changed the agent: live=%v total=%d", a.ComputeClockLive(), a.TotalComputeSeconds())
			}
		})
	}
}

// The guard on a same-boot checkpoint's kernel reading, built directly: a
// reading later than now cannot be resumed from, so the recorded total is
// carried instead.
func TestResolveComputeStart_SameBootCheckpointFromTheFutureIsNotResumed(t *testing.T) {
	now := time.Now()
	cp := &ComputeCheckpoint{
		InstanceID: hibInstance, BootID: hibBoot, BaseSeconds: 70, CountFrom: now.Add(-time.Minute),
		TotalSeconds: 130, KernelClock: true,
		CountFromMonotonicNS: int64(time.Hour), CountFromBoottimeNS: int64(time.Hour),
	}
	start := resolveComputeStart(computeStartInputs{
		InstanceID: hibInstance, BootID: hibBoot, SporedAt: now, Checkpoint: cp,
		Kernel: kernelClocks{Monotonic: time.Minute, Boottime: time.Minute}, KernelOK: true,
	})
	if start.Base != 130 || start.Origin.Monotonic != time.Minute {
		t.Errorf("base=%d origin=%v, want the recorded 130s counted on from now", start.Base, start.Origin)
	}
}

// The daemon itself counts on the kernel's monotonic clock from the moment it
// is built, and says so in the clock it publishes and the checkpoint it writes.
func TestNewAgent_DaemonCountsOnTheKernelClock(t *testing.T) {
	k := &fakeKernel{}
	k.install(t, hibBoot)
	p := queryProvider()
	p.config = &provider.Config{}
	a, err := newAgent(context.Background(), p, true)
	if err != nil {
		t.Fatal(err)
	}
	a.checkpointPath = "" // never touch the real /var/lib path
	k.run(30 * time.Second)
	k.hibernate(hibernation)
	if got := a.TotalComputeSeconds(); !withinSecond(got, 30) {
		t.Errorf("daemon compute = %ds, want the 30s the fake kernel ran", got)
	}
	if c := a.ComputeClock(); !c.KernelClock || c.BootID != hibBoot {
		t.Errorf("published clock carries no kernel reading: %+v", c)
	}
	if cp := a.computeCheckpoint(); !cp.KernelClock {
		t.Errorf("checkpoint carries no kernel reading: %+v", cp)
	}
}
