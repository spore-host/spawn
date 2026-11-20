package reaperdeploy

import (
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/iamcoverage"
	"github.com/spore-host/spawn/pkg/reaperiam"
	"golang.org/x/tools/go/ssa"
)

// selfRemovalEntry is the reaper's self-removal (lambda/ttl-reaper
// selfremoval.go). pkg/reaperiam's drift gates cut it from the reaper's
// inventory; this test holds its subtree to the role `spawn reaper deploy`
// writes.
const selfRemovalEntry = "(*github.com/spore-host/spawn/lambda/ttl-reaper.reaper).removeSelf"

// TestSelfRemovalCallsAreGranted: every AWS action the self-removal can call,
// discovered from the reaper's code (internal/iamcoverage), is granted by the
// self-hosted reaper's policy; and every action SelfRemovalStatements adds is
// one it calls, so the new grants are exactly what the code needs. The calls
// a reap would make (spawn#1657's one-shot jobs) are in the code already,
// behind effectiveSelfRemovalMode, so their grants are counted here.
func TestSelfRemovalCallsAreGranted(t *testing.T) {
	prog, err := iamcoverage.Load(iamcoverage.LoadConfig{
		Dir: "../../lambda/ttl-reaper", GOOS: "linux", GOARCH: "arm64", Patterns: []string{"."}, Root: "../..",
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := prog.Func(selfRemovalEntry)
	if err != nil {
		t.Fatal(err)
	}
	inv := prog.Calls([]*ssa.Function{entry}, nil)
	if len(inv.Unmapped) > 0 {
		t.Fatalf("unmapped AWS calls: %s", strings.Join(inv.Unmapped, "; "))
	}
	calls := map[string]bool{}
	for a := range inv.Actions {
		calls[a] = true
	}
	if len(calls) < 5 {
		t.Fatalf("discovered only %v under %s — the discovery is broken", inv.Sorted(), selfRemovalEntry)
	}

	granted := map[string]bool{}
	added := map[string]bool{}
	for _, st := range append(reaperiam.ScanSelfStatements(), reaperiam.OwnAccountStatements()...) {
		for _, a := range st.Action {
			granted[a] = true
		}
	}
	for _, st := range SelfRemovalStatements("123456789012", "my-own-bucket") {
		for _, a := range st.Action {
			granted[a], added[a] = true, true
		}
	}
	var missing, unused []string
	for a := range calls {
		if !granted[a] {
			missing = append(missing, a)
		}
	}
	for a := range added {
		if !calls[a] {
			unused = append(unused, a)
		}
	}
	sort.Strings(missing)
	sort.Strings(unused)
	if len(missing) > 0 {
		t.Errorf("the self-removal calls %s, which the self-hosted reaper's policy does not grant", strings.Join(missing, ", "))
	}
	if len(unused) > 0 {
		t.Errorf("SelfRemovalStatements grants %s, which the self-removal never calls", strings.Join(unused, ", "))
	}
	for _, must := range []string{"scheduler:CreateSchedule", "scheduler:CreateScheduleGroup", "scheduler:TagResource", "iam:PassRole"} {
		if !calls[must] {
			t.Errorf("the self-removal does not reach %s: the one-shot path is no longer in the code", must)
		}
	}
	t.Logf("self-removal AWS calls: %s", strings.Join(inv.Sorted(), ", "))
}
