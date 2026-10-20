package agent

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// releasingProvider is a stubProvider that can release its launch Elastic IP
// (#661) and records each release.
type releasingProvider struct {
	stubProvider
	releases int
}

func (r *releasingProvider) ReleaseOwnElasticIPs(_ context.Context) { r.releases++ }

// spotEIPAgent is a test agent on a releasing provider whose release goroutine
// runs inline and whose wait is recorded instead of slept.
func spotEIPAgent(t *testing.T, now time.Time) (*Agent, *releasingProvider, *[]time.Duration) {
	t.Helper()
	a := newTestAgent(t, nil)
	p := &releasingProvider{stubProvider: *a.provider.(*stubProvider)}
	a.provider = p
	var waits []time.Duration
	a.spotEIPNow = func() time.Time { return now }
	a.spotEIPSleep = func(_ context.Context, d time.Duration) { waits = append(waits, d) }
	a.spotEIPGo = func(f func()) { f() }
	return a, p, &waits
}

// A Spot terminate notice releases the address, once, spotEIPReleaseLead before
// the interruption time — not at the notice, which would cut the instance's only
// egress for the rest of the two-minute window.
func TestSpotTerminateNoticeReleasesTheElasticIPOnceNearTheDeadline(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a, p, waits := spotEIPAgent(t, now)
	info := &provider.InterruptionInfo{Action: "terminate", Time: now.Add(2 * time.Minute)}

	for i := 0; i < 3; i++ { // the monitor re-enters the handler every 5s
		a.scheduleSpotElasticIPRelease(context.Background(), info)
	}
	if p.releases != 1 {
		t.Fatalf("released %d times, want exactly once across repeated notices", p.releases)
	}
	if want := 2*time.Minute - spotEIPReleaseLead; len(*waits) != 1 || (*waits)[0] != want {
		t.Errorf("waited %v, want one wait of %v (until %v before the interruption)", *waits, want, spotEIPReleaseLead)
	}
}

// A notice whose deadline is already inside the lead releases at once.
func TestSpotTerminateNoticePastTheLeadReleasesImmediately(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a, p, waits := spotEIPAgent(t, now)
	a.scheduleSpotElasticIPRelease(context.Background(), &provider.InterruptionInfo{Action: "terminate", Time: now.Add(5 * time.Second)})
	if p.releases != 1 || len(*waits) != 0 {
		t.Errorf("releases=%d waits=%v, want an immediate release with no wait", p.releases, *waits)
	}
}

// A stop or hibernate interruption brings the instance back, and a stopped
// instance keeps its address: releasing it would leave the restarted instance
// unreachable.
func TestSpotStopOrHibernateNoticeKeepsTheElasticIP(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, action := range []string{"stop", "hibernate"} {
		a, p, _ := spotEIPAgent(t, now)
		a.scheduleSpotElasticIPRelease(context.Background(), &provider.InterruptionInfo{Action: action, Time: now.Add(2 * time.Minute)})
		if p.releases != 0 {
			t.Errorf("%s notice released the Elastic IP", action)
		}
	}
}

// The handler must actually call the scheduler. Structural, because the handler
// also broadcasts with wall and runs the pre-stop hook, which a unit test must
// not do: the call has to be in checkSpotInterruption's own body.
func TestTheSpotHandlerSchedulesTheElasticIPRelease(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "agent.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "checkSpotInterruption" || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "scheduleSpotElasticIPRelease" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "a" && len(call.Args) == 2 {
					if arg, ok := call.Args[1].(*ast.Ident); ok && arg.Name == "info" {
						found = true
					}
				}
			}
			return true
		})
	}
	if !found {
		t.Error("checkSpotInterruption does not call a.scheduleSpotElasticIPRelease(ctx, info): a Spot reclaim would leave the launch Elastic IP allocated and billing (#661)")
	}
}

// The production provider must have the capability the handler asserts for, or
// the type assertion fails silently and nothing is ever released.
var _ elasticIPReleaser = (*provider.EC2Provider)(nil)
