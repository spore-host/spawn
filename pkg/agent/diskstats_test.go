package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// diskstatsLine is one /proc/diskstats line as a 5.x kernel prints it: major,
// minor, name, then the I/O stats. Column 6 (index 5) is sectors read and column
// 10 (index 9) sectors written, in 512-byte units.
func diskstatsLine(major, minor int, name string, sectorsRead, sectorsWritten int64) string {
	return "  " + strconv.Itoa(major) + "  " + strconv.Itoa(minor) + " " + name +
		" 1234 10 " + strconv.FormatInt(sectorsRead, 10) + " 200 5678 30 " +
		strconv.FormatInt(sectorsWritten, 10) + " 400 0 600 800 0 0 0 0 0 0\n"
}

// sysBlockDisk is one entry of a fake /sys/block: a whole device, whether it is
// backed by hardware, and its partitions.
type sysBlockDisk struct {
	name       string
	hardware   bool
	partitions []string
}

// fakeSysBlock builds a /sys/block in a temp dir the way the kernel lays it out.
// Every entry is a symlink into /sys/devices; a hardware disk's directory holds a
// "device" link to its bus device, a virtual one's (loop, dm, md, zram) does not;
// partitions are subdirectories of their disk and have no /sys/block entry.
func fakeSysBlock(t *testing.T, disks ...sysBlockDisk) string {
	t.Helper()
	root := t.TempDir()
	sysBlock := filepath.Join(root, "block")
	mkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	symlink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(sysBlock)
	for i, d := range disks {
		var devDir string
		if d.hardware {
			bus := filepath.Join(root, "devices", "pci0000-00", "0000-00-0"+strconv.Itoa(i)+".0") // "-" not ":": Windows rejects ":" in names, and nothing reads them (#971)
			mkdir(bus)
			devDir = filepath.Join(bus, "block", d.name)
			mkdir(devDir)
			symlink(bus, filepath.Join(devDir, "device"))
		} else {
			devDir = filepath.Join(root, "devices", "virtual", "block", d.name)
			mkdir(devDir)
		}
		for _, p := range d.partitions {
			mkdir(filepath.Join(devDir, p))
		}
		symlink(devDir, filepath.Join(sysBlock, d.name))
	}
	return sysBlock
}

func writeProcDiskstats(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "diskstats")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Whole disks must be chosen by structure: every hardware disk counted once,
// whatever it is called, and no partition or virtual device counted at all (#937).
func TestReadDiskstatsBytes_CountsWholeDisksByStructure(t *testing.T) {
	// Noise present on most instances: loop devices (snaps), device-mapper (LVM,
	// LUKS), md RAID and zram. Each is a view of I/O already counted on a disk.
	noiseLines := diskstatsLine(7, 0, "loop0", 9000, 0) +
		diskstatsLine(7, 1, "loop1", 9000, 0) +
		diskstatsLine(253, 0, "dm-0", 7000, 7000) +
		diskstatsLine(9, 0, "md0", 6000, 6000) +
		diskstatsLine(252, 0, "zram0", 5000, 5000)
	noise := []sysBlockDisk{{name: "loop0"}, {name: "loop1"}, {name: "dm-0"}, {name: "md0"}, {name: "zram0"}}

	for _, tc := range []struct {
		name        string
		diskstats   string
		disks       []sysBlockDisk
		wantRead    int64
		wantWritten int64
		wantDevices []string
	}{
		{
			// Every Nitro instance. The old filter dropped nvme0n1 ("longer than 4
			// and ending in a digit") and so counted nothing.
			name: "nvme0n1 + nvme0n1p1 (Nitro)",
			diskstats: diskstatsLine(259, 0, "nvme0n1", 100000, 50000) +
				diskstatsLine(259, 1, "nvme0n1p1", 90000, 45000) +
				diskstatsLine(259, 2, "nvme0n1p128", 10, 0) + noiseLines,
			disks:       append([]sysBlockDisk{{name: "nvme0n1", hardware: true, partitions: []string{"nvme0n1p1", "nvme0n1p128"}}}, noise...),
			wantRead:    100000 * 512,
			wantWritten: 50000 * 512,
			wantDevices: []string{"nvme0n1"},
		},
		{
			name: "nvme0n1 + nvme1n1 (root + data volume)",
			diskstats: diskstatsLine(259, 0, "nvme0n1", 100, 10) +
				diskstatsLine(259, 1, "nvme0n1p1", 90, 9) +
				diskstatsLine(259, 2, "nvme1n1", 200, 20),
			disks: []sysBlockDisk{
				{name: "nvme0n1", hardware: true, partitions: []string{"nvme0n1p1"}},
				{name: "nvme1n1", hardware: true},
			},
			wantRead:    300 * 512,
			wantWritten: 30 * 512,
			wantDevices: []string{"nvme0n1", "nvme1n1"},
		},
		{
			name: "xvda + xvda1 (Xen)",
			diskstats: diskstatsLine(202, 0, "xvda", 50000, 30000) +
				diskstatsLine(202, 1, "xvda1", 49000, 29000) + noiseLines,
			disks:       append([]sysBlockDisk{{name: "xvda", hardware: true, partitions: []string{"xvda1"}}}, noise...),
			wantRead:    50000 * 512,
			wantWritten: 30000 * 512,
			wantDevices: []string{"xvda"},
		},
		{
			// The old filter kept sda1 (exactly four characters) and so counted
			// this disk twice.
			name: "sda + sda1",
			diskstats: diskstatsLine(8, 0, "sda", 25000, 15000) +
				diskstatsLine(8, 1, "sda1", 24000, 14000) + noiseLines,
			disks:       append([]sysBlockDisk{{name: "sda", hardware: true, partitions: []string{"sda1"}}}, noise...),
			wantRead:    25000 * 512,
			wantWritten: 15000 * 512,
			wantDevices: []string{"sda"},
		},
		{
			name: "vda + vda1 (virtio)",
			diskstats: diskstatsLine(254, 0, "vda", 15000, 10000) +
				diskstatsLine(254, 1, "vda1", 14000, 9000),
			disks:       []sysBlockDisk{{name: "vda", hardware: true, partitions: []string{"vda1"}}},
			wantRead:    15000 * 512,
			wantWritten: 10000 * 512,
			wantDevices: []string{"vda"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sys := fakeSysBlock(t, tc.disks...)
			read, written, devices, err := readDiskstatsBytes(writeProcDiskstats(t, tc.diskstats), sys)
			if err != nil {
				t.Fatalf("readDiskstatsBytes: %v", err)
			}
			if read != tc.wantRead || written != tc.wantWritten {
				t.Errorf("read, written = %d, %d; want %d, %d", read, written, tc.wantRead, tc.wantWritten)
			}
			if !reflect.DeepEqual(devices, tc.wantDevices) {
				t.Errorf("counted %v, want %v", devices, tc.wantDevices)
			}
		})
	}
}

// No whole disk is "could not measure", never a measured zero: a host with
// only loop and dm devices in view (a container, say) must not read as idle.
func TestReadDiskstatsBytes_NoWholeDiskIsUnknown(t *testing.T) {
	sys := fakeSysBlock(t, sysBlockDisk{name: "loop0"}, sysBlockDisk{name: "dm-0"})
	path := writeProcDiskstats(t, diskstatsLine(7, 0, "loop0", 1, 1)+diskstatsLine(253, 0, "dm-0", 1, 1))
	if _, _, _, err := readDiskstatsBytes(path, sys); !errors.Is(err, errNoWholeDisk) {
		t.Fatalf("err = %v, want errNoWholeDisk", err)
	}
	if _, _, _, err := readDiskstatsBytes(filepath.Join(t.TempDir(), "absent"), sys); err == nil {
		t.Error("an unreadable /proc/diskstats was not reported as an error")
	}
}

func stubDiskIOBytes(t *testing.T, samples ...int64) {
	t.Helper()
	i := 0
	orig := readDiskIOBytes
	readDiskIOBytes = func(context.Context) int64 {
		s := samples[len(samples)-1]
		if i < len(samples) {
			s = samples[i]
			i++
		}
		return s
	}
	t.Cleanup(func() { readDiskIOBytes = orig })
}

// The first sample has nothing to delta against and must say so; after that each
// sample is the I/O since the previous one, not the total since boot. THE bug:
// the since-boot total was compared with a per-minute threshold, so any disk
// that had booted read as busy for ever (#937).
func TestDiskIOBytes_IsADeltaAfterAFirstSample(t *testing.T) {
	const boot = int64(1_500_000_000) // ~1.5 GB read at boot, far over the threshold
	stubDiskIOBytes(t, boot, boot+4096, boot+4096, boot+4096+2_000_000)
	a := newTestAgent(t, nil)

	v, measured, why := a.diskIOBytes(context.Background())
	if measured {
		t.Error("the first sample reported a measurement; there is no delta to measure")
	}
	if v != diskAssumedActive || why == "" {
		t.Errorf("first sample = (%d, %q), want the assume-active value and a reason", v, why)
	}

	for i, want := range []int64{4096, 0, 2_000_000} {
		v, measured, why = a.diskIOBytes(context.Background())
		if !measured {
			t.Fatalf("sample %d was not a measurement (%q)", i+2, why)
		}
		if v != want {
			t.Errorf("sample %d = %d, want the delta %d (the since-boot total is %d)", i+2, v, want, boot)
		}
	}
}

// A zero counter is a valid reading, not a permanent first sample (the shape of
// the #919 network bug).
func TestDiskIOBytes_ZeroCounterIsNotAPermanentFirstSample(t *testing.T) {
	stubDiskIOBytes(t, 0, 0, 0)
	a := newTestAgent(t, nil)
	_, _, _ = a.diskIOBytes(context.Background())
	for i := 0; i < 2; i++ {
		if v, measured, why := a.diskIOBytes(context.Background()); !measured || v != 0 {
			t.Fatalf("sample %d = (%d, measured=%v, %q), want a measured 0", i+2, v, measured, why)
		}
	}
}

// A counter that goes backwards (a volume detached, so the counted set shrank)
// gives a negative delta, which is under every idle threshold. It is unknown,
// not idle, and the next sample deltas against the new baseline.
func TestDiskIOBytes_BackwardsCounterIsUnknown(t *testing.T) {
	stubDiskIOBytes(t, 5_000_000, 100, 100)
	a := newTestAgent(t, nil)
	_, _, _ = a.diskIOBytes(context.Background())
	if v, measured, why := a.diskIOBytes(context.Background()); measured || v != diskAssumedActive || why == "" {
		t.Errorf("backwards counter = (%d, measured=%v, %q), want unknown, assume-active and a reason", v, measured, why)
	}
	if v, measured, _ := a.diskIOBytes(context.Background()); !measured || v != 0 {
		t.Errorf("after the reset = (%d, measured=%v), want a measured 0 against the new baseline", v, measured)
	}
}

// Unknown is reported as unknown on every call, treated as active, and warned
// about once rather than once a minute.
func TestDiskIOBytes_UnknownIsAssumedActiveAndWarnedOnce(t *testing.T) {
	stubDiskIOBytes(t, -1)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := newTestAgent(t, nil)
	buf.Reset()
	for i := 0; i < 3; i++ {
		v, measured, why := a.diskIOBytes(context.Background())
		if measured {
			t.Fatalf("call %d: an unreadable counter reported a measurement", i+1)
		}
		if v != diskAssumedActive {
			t.Errorf("call %d: value = %d, want the assume-active %d", i+1, v, diskAssumedActive)
		}
		if why == "" {
			t.Errorf("call %d: no reason given for the unknown", i+1)
		}
	}
	if n := strings.Count(buf.String(), "WARNING"); n != 1 {
		t.Errorf("warning logged %d times over 3 calls, want once:\n%s", n, buf.String())
	}
}

// The assume-active value is only safe while it is over the idle threshold; an
// unknown that read as idle would stop a busy instance.
func TestDiskAssumedActiveIsOverTheIdleThreshold(t *testing.T) {
	if diskAssumedActive <= idleDiskBytesThreshold {
		t.Errorf("diskAssumedActive %d is not over idleDiskBytesThreshold %d", diskAssumedActive, idleDiskBytesThreshold)
	}
}
