package taskproto

import (
	"path"
	"strings"
)

// NICTargetGbps is the Go reading of the wrapper's NIC-class table
// (nicSizeClasses, writeNICTarget): the peak network bandwidth, in Gb/s, that
// the wrapper asks the AWS CLI's CRT client to fill for instanceType. spored's
// own S3 uploads size themselves from it (`spored provenance upload`, #1324), so
// stage-out through spored and stage-out through the CLI read one table.
//
// It matches the shell exactly: the size is everything after the first dot,
// matched with the same case patterns in the same order, and a family is
// network-optimized when it has an "n" after a digit. A type the CRT client
// detects by itself (crtOptimizedTypes), for which the wrapper writes no
// target, is given the table's largest figure: it is at least that fast.
// An unknown size, or an empty type, is 0.
func NICTargetGbps(instanceType string) int {
	for _, t := range crtOptimizedTypes {
		if instanceType == t {
			top := 0
			for _, c := range nicSizeClasses {
				top = max(top, c.Gbps, c.NetOptGbps)
			}
			return top
		}
	}
	family, size, ok := strings.Cut(instanceType, ".")
	if !ok {
		return 0
	}
	netOpt, _ := path.Match("*[0-9]*n*", family)
	for _, c := range nicSizeClasses {
		for _, pat := range c.Sizes {
			if m, _ := path.Match(pat, size); m {
				if netOpt {
					return c.NetOptGbps
				}
				return c.Gbps
			}
		}
	}
	return 0
}
