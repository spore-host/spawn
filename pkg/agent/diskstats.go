package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Linux disk counters for the idle check and the metrics endpoint (#937).
//
// Like netdev.go this file is not build-tagged although only Linux has
// /proc/diskstats: the parsing is pure, so it is tested on every platform.
// sys_linux.go wires it to the real paths.

// errNoWholeDisk means /proc/diskstats was read but no device in it is a whole
// disk backed by hardware. It is "could not measure", never "measured zero".
var errNoWholeDisk = errors.New("no whole block device found")

// diskSectorBytes is the unit of the sector columns in /proc/diskstats. It is
// 512 whatever the device's real sector size: the kernel always reports in
// 512-byte units there.
const diskSectorBytes = 512

// isWholeDisk reports whether the named /proc/diskstats device is a whole disk
// backed by a device, by structure rather than by name.
//
// /sys/block lists whole block devices only; partitions live underneath their
// disk (/sys/block/nvme0n1/nvme0n1p1) and have no entry of their own, so
// counting a disk and its partitions twice is impossible. Of the whole devices,
// the real ones have a /sys/block/<name>/device link to their bus device, while
// loop, ram, zram, device-mapper (dm-*, LVM, LUKS) and md RAID devices do not:
// they are views of I/O that is already counted on the disks beneath them.
//
// Matching names was the bug. The old check accepted the prefixes xvd, nvme, sd
// and vd, then dropped "partitions" by "longer than four characters and ending
// in a digit". That dropped nvme0n1, the root disk on every Nitro instance, and
// kept sda1 and vda1, counting those disks twice (#937).
func isWholeDisk(sysBlock, name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return false
	}
	_, err := os.Stat(filepath.Join(sysBlock, name, "device"))
	return err == nil
}

// sumDiskstats sums bytes read and written in a /proc/diskstats dump over the
// devices wholeDisk accepts, returning the names it counted.
func sumDiskstats(procDiskstats []byte, wholeDisk func(name string) bool) (read, written int64, devices []string) {
	for _, line := range strings.Split(string(procDiskstats), "\n") {
		// major minor name reads merged sectors_read ms writes merged sectors_written ...
		fields := strings.Fields(line)
		if len(fields) < 10 || !wholeDisk(fields[2]) {
			continue
		}
		r, rerr := strconv.ParseInt(fields[5], 10, 64)
		w, werr := strconv.ParseInt(fields[9], 10, 64)
		if rerr != nil || werr != nil {
			continue
		}
		read += r * diskSectorBytes
		written += w * diskSectorBytes
		devices = append(devices, fields[2])
	}
	return read, written, devices
}

// readDiskstatsBytes reads cumulative bytes read and written over the whole
// disks. It returns errNoWholeDisk rather than zeros when none is found, so the
// caller cannot mistake "nothing counted" for "no I/O".
func readDiskstatsBytes(procDiskstats, sysBlock string) (read, written int64, devices []string, err error) {
	data, err := os.ReadFile(procDiskstats)
	if err != nil {
		return 0, 0, nil, err
	}
	read, written, devices = sumDiskstats(data, func(name string) bool {
		return isWholeDisk(sysBlock, name)
	})
	if len(devices) == 0 {
		return 0, 0, nil, errNoWholeDisk
	}
	return read, written, devices, nil
}
