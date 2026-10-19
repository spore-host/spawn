package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// /proc/net/dev as the kernel prints it: two header lines, then one line per
// interface. Column 1 after the colon is RX bytes, column 9 is TX bytes.
const procNetDevHeader = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
`

func netDevLine(name string, rx, tx string) string {
	return "  " + name + ": " + rx + " 10 0 0 0 0 0 0 " + tx + " 20 0 0 0 0 0 0\n"
}

// fakeSysClassNet builds a /sys/class/net in a temp dir. Physical interfaces get
// a device symlink into a fake bus, as the kernel creates; virtual ones are a
// bare directory, as the kernel creates for lo, bridges, veth pairs and tun.
//
// The bus and device directories are named like the kernel's ("pci0000:00",
// "0000:27:00.0") but with "-" for ":", which Windows refuses in a file name
// (#971). Nothing reads those names — the parser only asks whether a device
// link resolves — so the fixture loses nothing by it.
func fakeSysClassNet(t *testing.T, physical []string, virtual []string) string {
	t.Helper()
	root := t.TempDir()
	classNet := filepath.Join(root, "class", "net")
	bus := filepath.Join(root, "devices", "pci0000-00")
	for _, d := range []string{classNet, bus} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range physical {
		dev := filepath.Join(bus, "0000-27-0"+string(rune('0'+i))+".0")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(classNet, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dev, filepath.Join(classNet, name, "device")); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range virtual {
		if err := os.MkdirAll(filepath.Join(classNet, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return classNet
}

func writeProcNetDev(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(p, []byte(procNetDevHeader+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The interface set must be chosen by structure, so the NIC is counted whatever
// it is called and nothing virtual is (#919).
func TestReadNetDevBytes_CountsPhysicalInterfacesWhateverTheirName(t *testing.T) {
	for _, tc := range []struct {
		name             string
		procNetDev       string
		physical         []string
		virtual          []string
		wantRx, wantTx   int64
		wantInterfaceSet []string
	}{
		{
			// The reported instance: m8a.4xlarge, AL2023. The old name filter
			// matched neither "eth0" nor "ens" here and counted nothing.
			name: "enp39s0 only (m8a, AL2023)",
			procNetDev: netDevLine("lo", "5000", "5000") +
				netDevLine("enp39s0", "217684698", "2925026"),
			physical:         []string{"enp39s0"},
			virtual:          []string{"lo"},
			wantRx:           217684698,
			wantTx:           2925026,
			wantInterfaceSet: []string{"enp39s0"},
		},
		{
			name: "ens5 (Nitro, AL2)",
			procNetDev: netDevLine("lo", "5000", "5000") +
				netDevLine("ens5", "1000", "2000"),
			physical:         []string{"ens5"},
			virtual:          []string{"lo"},
			wantRx:           1000,
			wantTx:           2000,
			wantInterfaceSet: []string{"ens5"},
		},
		{
			// Large counters on older kernels run into the colon: "eth0:4294967296".
			name:             "eth0 with no space after the colon (Xen, older kernels)",
			procNetDev:       "    lo:5000 1 0 0 0 0 0 0 5000 1 0 0 0 0 0 0\n  eth0:4294967296 1 0 0 0 0 0 0 300 2 0 0 0 0 0 0\n",
			physical:         []string{"eth0"},
			virtual:          []string{"lo"},
			wantRx:           4294967296,
			wantTx:           300,
			wantInterfaceSet: []string{"eth0"},
		},
		{
			// Container traffic crosses docker0 AND a veth AND (on the way out) the
			// NIC; counting the virtual ones would count it two or three times, and
			// count container-to-container chatter as instance activity.
			name: "docker0, veth, bridge, tun and tailscale are not counted",
			procNetDev: netDevLine("lo", "9", "9") +
				netDevLine("ens5", "1000", "2000") +
				netDevLine("docker0", "777777", "777777") +
				netDevLine("veth1a2b3c", "888888", "888888") +
				netDevLine("br-0123abcd", "999999", "999999") +
				netDevLine("tun0", "555555", "555555") +
				netDevLine("tailscale0", "444444", "444444"),
			physical:         []string{"ens5"},
			virtual:          []string{"lo", "docker0", "veth1a2b3c", "br-0123abcd", "tun0", "tailscale0"},
			wantRx:           1000,
			wantTx:           2000,
			wantInterfaceSet: []string{"ens5"},
		},
		{
			name: "every physical interface is summed (multi-ENI)",
			procNetDev: netDevLine("lo", "9", "9") +
				netDevLine("enp39s0", "100", "10") +
				netDevLine("enp40s0", "200", "20"),
			physical:         []string{"enp39s0", "enp40s0"},
			virtual:          []string{"lo"},
			wantRx:           300,
			wantTx:           30,
			wantInterfaceSet: []string{"enp39s0", "enp40s0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sys := fakeSysClassNet(t, tc.physical, tc.virtual)
			rx, tx, ifaces, err := readNetDevBytes(writeProcNetDev(t, tc.procNetDev), sys)
			if err != nil {
				t.Fatalf("readNetDevBytes: %v", err)
			}
			if rx != tc.wantRx || tx != tc.wantTx {
				t.Errorf("rx, tx = %d, %d; want %d, %d", rx, tx, tc.wantRx, tc.wantTx)
			}
			if !reflect.DeepEqual(ifaces, tc.wantInterfaceSet) {
				t.Errorf("counted %v, want %v", ifaces, tc.wantInterfaceSet)
			}
		})
	}
}

// No physical interface is "could not measure", never a measured zero. A zero
// here is what made every sample look like a first sample (#919).
func TestReadNetDevBytes_NoPhysicalInterfaceIsUnknown(t *testing.T) {
	sys := fakeSysClassNet(t, nil, []string{"lo", "docker0"})
	path := writeProcNetDev(t, netDevLine("lo", "5000", "5000")+netDevLine("docker0", "1", "1"))
	_, _, _, err := readNetDevBytes(path, sys)
	if !errors.Is(err, errNoPhysicalInterface) {
		t.Fatalf("err = %v, want errNoPhysicalInterface", err)
	}

	if _, _, _, err := readNetDevBytes(filepath.Join(t.TempDir(), "absent"), sys); err == nil {
		t.Error("an unreadable /proc/net/dev was not reported as an error")
	}
}

func stubNetworkBytes(t *testing.T, samples ...[2]int64) {
	t.Helper()
	i := 0
	orig := readNetworkBytes
	readNetworkBytes = func(context.Context) (int64, int64) {
		s := samples[len(samples)-1]
		if i < len(samples) {
			s = samples[i]
			i++
		}
		return s[0], s[1]
	}
	t.Cleanup(func() { readNetworkBytes = orig })
}

// The first sample has nothing to delta against and must say so; the second is
// a measurement.
func TestNetworkBytes_FirstSampleIsUnknownThenMeasured(t *testing.T) {
	stubNetworkBytes(t, [2]int64{217684698, 2925026}, [2]int64{217699919, 2963423})
	a := newTestAgent(t, nil)

	v, measured, why := a.networkBytes(context.Background())
	if measured {
		t.Error("the first sample reported a measurement; there is no delta to measure")
	}
	if v != networkAssumedActive || why == "" {
		t.Errorf("first sample = (%d, %q), want the assume-active value and a reason", v, why)
	}

	v, measured, _ = a.networkBytes(context.Background())
	if !measured {
		t.Fatal("the second sample was not reported as a measurement")
	}
	// The reported instance's real traffic over the sampled interval.
	if want := int64((217699919 - 217684698) + (2963423 - 2925026)); v != want {
		t.Errorf("delta = %d, want %d", v, want)
	}
}

// THE bug. The first-sample test was prev == 0, so a counter that read 0 on every
// call made every call a "first call", returning the assume-active value forever
// and keeping idle-stop from ever firing. A counter of zero is a valid reading.
func TestNetworkBytes_ZeroCountersAreNotAPermanentFirstSample(t *testing.T) {
	stubNetworkBytes(t, [2]int64{0, 0}, [2]int64{0, 0}, [2]int64{0, 0})
	a := newTestAgent(t, nil)
	_, _, _ = a.networkBytes(context.Background()) // first sample
	for i := 0; i < 2; i++ {
		v, measured, why := a.networkBytes(context.Background())
		if !measured || v != 0 {
			t.Fatalf("sample %d = (%d, measured=%v, %q), want a measured 0 — "+
				"a zero counter was taken for the first sample again (#919)", i+2, v, measured, why)
		}
	}
}

// Unknown is reported as unknown on every call, treated as active, and warned
// about once rather than once a minute.
func TestNetworkBytes_UnknownIsAssumedActiveAndWarnedOnce(t *testing.T) {
	stubNetworkBytes(t, [2]int64{-1, -1})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := newTestAgent(t, nil)
	buf.Reset()
	for i := 0; i < 3; i++ {
		v, measured, why := a.networkBytes(context.Background())
		if measured {
			t.Fatalf("call %d: an unreadable counter reported a measurement", i+1)
		}
		if v != networkAssumedActive {
			t.Errorf("call %d: value = %d, want the assume-active %d", i+1, v, networkAssumedActive)
		}
		if why == "" {
			t.Errorf("call %d: no reason given for the unknown", i+1)
		}
	}
	if n := strings.Count(buf.String(), "WARNING"); n != 1 {
		t.Errorf("warning logged %d times over 3 calls, want once:\n%s", n, buf.String())
	}
}

// A counter that goes backwards (an ENI detached, a driver reset) gives a
// negative delta, which is under every idle threshold. It is unknown, not idle.
func TestNetworkBytes_BackwardsCounterIsUnknown(t *testing.T) {
	stubNetworkBytes(t, [2]int64{5000000, 5000000}, [2]int64{100, 100})
	a := newTestAgent(t, nil)
	_, _, _ = a.networkBytes(context.Background())
	if v, measured, _ := a.networkBytes(context.Background()); measured || v < 0 {
		t.Errorf("backwards counter = (%d, measured=%v), want unknown and assume-active", v, measured)
	}
}
