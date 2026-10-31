package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// The --no-public-ip pre-flight (#1436) refuses a region with no
// spawn-binaries bucket of its own, because the bootstrap's only source there
// is the us-east-1 fallback, which no VPC endpoint in the region serves. Its
// list of regions must be the one the release workflow really publishes to,
// or it refuses a region that has a bucket or passes one that has none.
func TestPublishedRegionsAreTheReleaseBuckets(t *testing.T) {
	var released []string
	for _, b := range strings.Fields(releaseJobWithStep(t).Env["SPAWN_BINARY_BUCKETS"]) {
		r, ok := strings.CutPrefix(b, "spawn-binaries-")
		if !ok {
			t.Fatalf("SPAWN_BINARY_BUCKETS lists %q, which is not a spawn-binaries-<region> bucket", b)
		}
		released = append(released, r)
	}
	if len(released) == 0 {
		t.Fatal("SPAWN_BINARY_BUCKETS lists no bucket; discovery has broken")
	}
	got := sporedinstall.PublishedRegions()
	sort.Strings(released)
	sort.Strings(got)
	if !reflect.DeepEqual(got, released) {
		t.Errorf("sporedinstall.PublishedRegions() = %v, but the release workflow publishes to %v", got, released)
	}
	if !sporedinstall.Published(sporedinstall.FallbackRegion) {
		t.Errorf("the fallback region %s has no published bucket", sporedinstall.FallbackRegion)
	}
}
