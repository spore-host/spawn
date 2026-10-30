package iamcoverage

import (
	"sort"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// TestPrivateServicesCoverEverySporedService is #1433's discovery gate. A
// --no-public-ip launch is refused unless its subnet reaches every service
// spored calls, through a VPC endpoint or a NAT route, and that list is
// aws.PrivateServices. It must not be written from memory: every IAM service
// prefix spored's call graph reaches (every spored principal, from the same
// RTA/VTA inventory the IAM coverage gate uses, so a call through an
// interface, a helper package or a paginator counts) must be served by an
// entry, and every entry's prefix must still be reached. A service spored
// starts calling fails here until the pre-flight knows how a private instance
// reaches it.
func TestPrivateServicesCoverEverySporedService(t *testing.T) {
	served := map[string]string{}
	for _, s := range spawnaws.PrivateServices() {
		for _, p := range s.IAMPrefixes {
			served[p] = s.Endpoint
		}
	}
	invs := inventories(t)
	reached := map[string][]string{}
	principalsSeen := 0
	for _, p := range principals {
		if p.build.dir != sporedBuild.dir || p.build.pkg != sporedBuild.pkg {
			continue
		}
		principalsSeen++
		inv := invs[p.name]
		if inv == nil || len(inv.actions) == 0 {
			t.Errorf("%s: discovered no AWS calls — the discovery has broken, not spored", p.name)
			continue
		}
		for a := range inv.actions {
			pre := strings.SplitN(a, ":", 2)[0]
			reached[pre] = append(reached[pre], p.name+": "+a)
		}
	}
	if principalsSeen == 0 || len(reached) == 0 {
		t.Fatal("found no spored principal or no spored AWS call; the discovery has broken")
	}
	prefixes := make([]string, 0, len(reached))
	for p := range reached {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, pre := range prefixes {
		if _, ok := served[pre]; !ok {
			sort.Strings(reached[pre])
			t.Errorf("spored calls %s (%s), and no aws.PrivateServices entry serves IAM prefix %q: a --no-public-ip launch "+
				"would pass the subnet check and spored's call would fail on the instance. Add an entry saying which VPC "+
				"endpoint serves it (or that only NAT does) and when a launch needs it", pre, strings.Join(reached[pre], ", "), pre)
		}
	}
	for pre, endpoint := range served {
		if _, ok := reached[pre]; !ok {
			t.Errorf("aws.PrivateServices names IAM prefix %q (endpoint %q), which spored's call graph no longer reaches: "+
				"drop the prefix, or the entry if nothing else needs it", pre, endpoint)
		}
	}
}
