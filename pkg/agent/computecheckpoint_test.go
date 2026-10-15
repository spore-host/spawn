package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// #928's timeline: launched, ran about a minute, stopped about two minutes,
// then restarted. The restarted daemon reported 22s of compute; it should have
// carried the first run over.
var (
	t0928       = time.Date(2026, 10, 15, 17, 42, 0, 0, time.UTC)
	boot1At     = t0928.Add(10 * time.Second) // kernel up
	spored1At   = t0928.Add(40 * time.Second) // cloud-init installed and started spored
	stoppedAt   = t0928.Add(80 * time.Second)
	boot2At     = stoppedAt.Add(2 * time.Minute)
	spored2At   = boot2At.Add(15 * time.Second)
	firstBootID = "boot-1"
	nextBootID  = "boot-2"
)

const issueInstance = "i-0abc928"

// resolveWall is resolveComputeStart's wall-clock decision, for the rules that
// predate the kernel clocks (#945) and still apply where there are none.
func resolveWall(in computeStartInputs) (base int64, from time.Time, source string) {
	s := resolveComputeStart(in)
	return s.Base, s.From, s.Source
}

// The first boot's checkpoint as the daemon's shutdown wrote it: everything
// from kernel boot to the stop.
func firstBootCheckpoint() *ComputeCheckpoint {
	return &ComputeCheckpoint{
		InstanceID: issueInstance, BootID: firstBootID,
		BaseSeconds: 0, CountFrom: boot1At,
		TotalSeconds: int64(stoppedAt.Sub(boot1At).Seconds()), WrittenAt: stoppedAt,
	}
}

// Cumulative compute must persist across a stop/start (#928): the restarted
// daemon carries the first boot's running time, and counts the new boot from
// the kernel's boot time — the OS and cloud-init time EC2 bills, which counting
// from spored's own start dropped.
func TestResolveComputeStart_CarriesEarlierBootsAcrossAStop(t *testing.T) {
	base, from, _ := resolveWall(computeStartInputs{
		InstanceID: issueInstance, BootID: nextBootID, BootTime: boot2At,
		LaunchTime: t0928, SporedAt: spored2At,
		TagSeconds: 0, // the shutdown's tag write was lost
		Checkpoint: firstBootCheckpoint(),
	})
	if want := int64(stoppedAt.Sub(boot1At).Seconds()); base != want {
		t.Errorf("carried base = %ds, want the first boot's %ds", base, want)
	}
	if !from.Equal(boot2At) {
		t.Errorf("this boot counts from %v, want the kernel boot %v", from, boot2At)
	}
	// 22s after the second boot (the issue's measurement), the total is the
	// first run plus 22s — not 22s.
	at := boot2At.Add(22 * time.Second)
	if total := base + int64(at.Sub(from).Seconds()); total != 70+22 {
		t.Errorf("total at +22s = %ds, want %ds", total, 70+22)
	}
}

// Neither copy is authoritative: whichever recorded more wins, since neither
// includes the current boot.
func TestResolveComputeStart_TheLargerOfTagAndCheckpointWins(t *testing.T) {
	base, _, _ := resolveWall(computeStartInputs{
		InstanceID: issueInstance, BootID: nextBootID, BootTime: boot2At, SporedAt: spored2At,
		TagSeconds: 500, Checkpoint: firstBootCheckpoint(),
	})
	if base != 500 {
		t.Errorf("base = %d, want the tag's newer 500", base)
	}
}

// A spored restart WITHIN a boot (an in-place upgrade, a crash) must not count
// the boot's time twice: it resumes the boot's count where it began.
func TestResolveComputeStart_RestartWithinABootDoesNotDoubleCount(t *testing.T) {
	cp := &ComputeCheckpoint{
		InstanceID: issueInstance, BootID: nextBootID, BaseSeconds: 70, CountFrom: boot2At,
		TotalSeconds: 70 + 600, WrittenAt: boot2At.Add(10 * time.Minute),
	}
	restart := boot2At.Add(10*time.Minute + 5*time.Second)
	base, from, _ := resolveWall(computeStartInputs{
		InstanceID: issueInstance, BootID: nextBootID, BootTime: boot2At, SporedAt: restart,
		TagSeconds: 70 + 600, // the stop flush wrote the total INCLUDING this boot
		Checkpoint: cp,
	})
	if base != 70 || !from.Equal(boot2At) {
		t.Errorf("base=%d from=%v, want 70 from %v", base, from, boot2At)
	}
	if total := base + int64(restart.Sub(from).Seconds()); total != 70+605 {
		t.Errorf("total after restart = %d, want %d (no double count)", total, 70+605)
	}
}

// With no checkpoint but a non-zero tag, the tag may already include part of
// this boot (an older spored ran in it), so this boot counts only from spored's
// start — never from the kernel's boot, which would count that part twice.
func TestResolveComputeStart_TagOnlyCountsFromSporedStart(t *testing.T) {
	base, from, _ := resolveWall(computeStartInputs{
		InstanceID: issueInstance, BootID: nextBootID, BootTime: boot2At, SporedAt: spored2At, TagSeconds: 300,
	})
	if base != 300 || !from.Equal(spored2At) {
		t.Errorf("base=%d from=%v, want 300 from spored's start %v", base, from, spored2At)
	}
}

// A checkpoint that cannot describe this instance, or cannot say which boot it
// is from, is not used.
func TestResolveComputeStart_IgnoresAForeignOrUnidentifiedCheckpoint(t *testing.T) {
	foreign := firstBootCheckpoint()
	foreign.InstanceID = "i-baked-into-the-ami"
	noBoot := firstBootCheckpoint()
	noBoot.BootID = ""
	for name, in := range map[string]computeStartInputs{
		"other instance":       {InstanceID: issueInstance, BootID: nextBootID, SporedAt: spored2At, TagSeconds: 0, Checkpoint: foreign},
		"checkpoint has no id": {InstanceID: issueInstance, BootID: nextBootID, SporedAt: spored2At, TagSeconds: 0, Checkpoint: noBoot},
		"this boot has no id":  {InstanceID: issueInstance, BootID: "", SporedAt: spored2At, TagSeconds: 0, Checkpoint: firstBootCheckpoint()},
	} {
		if base, _, _ := resolveWall(in); base != 0 {
			t.Errorf("%s: base = %d, want the checkpoint ignored", name, base)
		}
	}
}

// First boot: nothing recorded, so the boot counts from the kernel's boot —
// but never from before the instance launched.
func TestResolveComputeStart_FirstBoot(t *testing.T) {
	_, from, _ := resolveWall(computeStartInputs{
		InstanceID: issueInstance, BootID: firstBootID, BootTime: boot1At, LaunchTime: t0928, SporedAt: spored1At,
	})
	if !from.Equal(boot1At) {
		t.Errorf("from = %v, want boot %v", from, boot1At)
	}
	_, from, _ = resolveWall(computeStartInputs{
		InstanceID: issueInstance, BootID: firstBootID, BootTime: t0928.Add(-time.Hour), LaunchTime: t0928, SporedAt: spored1At,
	})
	if !from.Equal(t0928) {
		t.Errorf("from = %v, want clamped to launch %v", from, t0928)
	}
}

// The stop path loses nothing (#928): Cleanup — what a stop's SIGTERM runs —
// writes the local checkpoint even when the network tag write fails, retries
// that write, and the next boot carries the checkpoint's total.
func TestCleanup_PersistsComputeWhenTheTagWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compute-checkpoint.json")
	tagger := &fakeTagPutter{err: errors.New("no route to IMDS")}
	identity := &provider.Identity{InstanceID: issueInstance, Region: "us-west-2", Provider: "ec2"}
	a := &Agent{
		identity:       identity,
		config:         &provider.Config{},
		startTime:      time.Now().Add(-40 * time.Second),
		computeStart:   time.Now().Add(-70 * time.Second),
		bootID:         firstBootID,
		checkpointPath: path,
		tagger:         tagger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.Cleanup(ctx)

	if tagger.calls < 2 {
		t.Errorf("tag write attempted %d time(s); a failed shutdown write must be retried", tagger.calls)
	}
	cp, err := ReadComputeCheckpoint(path)
	if err != nil {
		t.Fatalf("Cleanup left no checkpoint: %v", err)
	}
	if cp.TotalSeconds < 70 || cp.BootID != firstBootID || cp.InstanceID != issueInstance {
		t.Fatalf("checkpoint = %+v, want ≥70s for this boot and instance", cp)
	}

	// The next boot.
	base, _, _ := resolveWall(computeStartInputs{
		InstanceID: issueInstance, BootID: nextBootID, BootTime: time.Now(), SporedAt: time.Now(),
		TagSeconds: 0, Checkpoint: &cp,
	})
	if base < 70 {
		t.Errorf("next boot carried %ds, want the ≥70s the stop recorded", base)
	}
}

// The tag flush writes the agent's total through its tag writer.
func TestFlushComputeSecondsTag_WritesTheTotal(t *testing.T) {
	tagger := &fakeTagPutter{}
	a := &Agent{
		identity:           &provider.Identity{InstanceID: issueInstance, Region: "us-west-2", Provider: "ec2"},
		startTime:          time.Now(),
		computeStart:       time.Now().Add(-30 * time.Second),
		computeSecondsBase: 70,
		tagger:             tagger,
	}
	a.flushComputeSecondsTag(context.Background())
	if got := tagger.tags["spawn:compute-seconds"]; got != "100" && got != "101" {
		t.Errorf("spawn:compute-seconds = %q, want base 70 + ~30s", got)
	}
}
