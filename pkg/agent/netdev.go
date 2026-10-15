package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Linux network counters for the idle check and the metrics endpoint (#919).
//
// This file is not build-tagged although only Linux has /proc/net/dev: the
// parsing is pure, so it is tested on every platform, including the darwin
// machines most development happens on. sys_linux.go wires it to the real paths.

// errNoPhysicalInterface means /proc/net/dev was read but no interface in it is
// backed by a device. It is "could not measure", never "measured zero".
var errNoPhysicalInterface = errors.New("no physical network interface found")

// isPhysicalInterface reports whether the named interface is backed by a device,
// by structure rather than by name: a NIC has a /sys/class/net/<name>/device link
// to its bus device, and lo, bridges, veth pairs, tun/tap, docker0 and VPN
// interfaces do not.
//
// Matching names was the bug. The old check accepted only "eth0" or "ens*", so
// the enp39s0 on m8a/AL2023 read as zero bytes forever (#919). Predictable names
// vary by family and AMI (eth*, ens*, enp*s*, enX*), and any list of them is a
// list of the ones somebody has seen.
func isPhysicalInterface(sysClassNet, name string) bool {
	_, err := os.Stat(filepath.Join(sysClassNet, name, "device"))
	return err == nil
}

// sumNetDev sums RX and TX bytes in a /proc/net/dev dump over the interfaces
// physical accepts, returning the names it counted.
func sumNetDev(procNetDev []byte, physical func(name string) bool) (rx, tx int64, ifaces []string) {
	for _, line := range strings.Split(string(procNetDev), "\n") {
		// "  enp39s0: 217684698 1234 ... 2925026 ..." — split on the colon, not on
		// whitespace: with large counters older kernels print "eth0:217684698".
		name, counters, ok := strings.Cut(line, ":")
		if !ok {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(counters)
		if len(fields) < 9 || !physical(name) {
			continue
		}
		r, rerr := strconv.ParseInt(fields[0], 10, 64)
		t, terr := strconv.ParseInt(fields[8], 10, 64)
		if rerr != nil || terr != nil {
			continue
		}
		rx += r
		tx += t
		ifaces = append(ifaces, name)
	}
	return rx, tx, ifaces
}

// readNetDevBytes reads cumulative RX/TX bytes over the physical interfaces. It
// returns errNoPhysicalInterface rather than zeros when none is found, so the
// caller cannot mistake "nothing counted" for "no traffic".
func readNetDevBytes(procNetDev, sysClassNet string) (rx, tx int64, ifaces []string, err error) {
	data, err := os.ReadFile(procNetDev)
	if err != nil {
		return 0, 0, nil, err
	}
	rx, tx, ifaces = sumNetDev(data, func(name string) bool {
		return isPhysicalInterface(sysClassNet, name)
	})
	if len(ifaces) == 0 {
		return 0, 0, nil, errNoPhysicalInterface
	}
	return rx, tx, ifaces, nil
}
