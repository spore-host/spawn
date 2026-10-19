package agent

import (
	"context"
	"testing"
	"time"
)

// Readings from instances nobody was using, taken from spored's own log on the
// supported AMIs (#1093): AL2023 arm64 and x86_64 and Ubuntu 24.04, t-class,
// us-east-1, untouched for 20 minutes. These minutes are what idle looks like,
// so each must read idle. With both thresholds at 100 KB, every one of them read
// active and --idle-timeout could never fire on any Linux AMI.
var measuredIdleMinutes = []struct {
	ami       string
	disk, net int64
}{
	{"AL2023 arm64 (journald)", 7778304, 60000},
	{"AL2023 arm64", 1273856, 131687},
	{"AL2023 arm64", 4599808, 145343},
	{"AL2023 x86_64", 3911680, 148430},
	{"AL2023 x86_64", 1277952, 123603},
	{"AL2023 t4g.small (first test)", 4288512, 186387},
	{"Ubuntu 24.04", 3674112, 54428},
	{"Ubuntu 24.04", 139264, 21471},
}

func TestMeasuredIdleMinutesReadIdle(t *testing.T) {
	noSessions(t)
	a := newTestAgent(t, nil)
	stubAccelerators(t, nil, func(string) ([]byte, error) { return nil, nil })
	for _, m := range measuredIdleMinutes {
		idle, reason := a.idleDecision(context.Background(), idleSnapshot{
			taken: time.Now(), cpu: 0.5, cpuMeasured: true,
			net: m.net, netMeasured: true, disk: m.disk, diskMeasured: true,
		})
		if !idle {
			t.Errorf("%s: an untouched minute (disk %d B, network %d B) read as active: %s", m.ami, m.disk, m.net, reason)
		}
	}
}

// Work clears the thresholds: the levels below are a job, not background, and
// must keep the instance up even with idle CPU.
func TestWorkloadMinutesReadActive(t *testing.T) {
	noSessions(t)
	a := newTestAgent(t, nil)
	stubAccelerators(t, nil, func(string) ([]byte, error) { return nil, nil })
	for _, w := range []struct {
		what      string
		disk, net int64
	}{
		{"writing results at 1 MB/s", 60 << 20, 0},
		{"downloading a dataset at 1 MB/s", 0, 60 << 20},
		{"a checkpoint flush", 512 << 20, 0},
		{"syncing outputs to S3 at 100 KB/s", 0, 6 << 20},
	} {
		idle, _ := a.idleDecision(context.Background(), idleSnapshot{
			taken: time.Now(), cpu: 0.5, cpuMeasured: true,
			net: w.net, netMeasured: true, disk: w.disk, diskMeasured: true,
		})
		if idle {
			t.Errorf("%s (disk %d B, network %d B per minute) read as idle", w.what, w.disk, w.net)
		}
	}
}

// The assume-active placeholders must stay far over the thresholds, so an
// unmeasured reading can never look idle even to code that skips the measured
// flag.
func TestAssumedActiveIsOverTheThresholds(t *testing.T) {
	if networkAssumedActive <= idleNetworkBytesThreshold || diskAssumedActive <= idleDiskBytesThreshold {
		t.Errorf("assumed-active placeholders (net %d, disk %d) are not over the thresholds (%d, %d)",
			networkAssumedActive, diskAssumedActive, idleNetworkBytesThreshold, idleDiskBytesThreshold)
	}
}
