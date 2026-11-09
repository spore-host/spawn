package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// spawn doctor's sweep role check (#1537): what each state of
// SpawnSweepCrossAccountRole reports.
func TestSweepRoleVerdict(t *testing.T) {
	none := func(context.Context) (string, error) { return "", nil }
	mine := aws.SweepRolePolicySchema()
	principals := []string{"arn:aws:iam::966362334030:role/SpawnSweepOrchestratorRole"}
	cases := []struct {
		name   string
		acct   string
		st     aws.SweepRoleState
		err    error
		mgmt   aws.ManagementAccountLookup
		pass   bool
		fail   bool
		saying string
	}{
		{name: "current", acct: "435415984226", st: aws.SweepRoleState{Exists: true, Schema: mine, Principals: principals}, pass: true, saying: "(current)"},
		{name: "newer", acct: "435415984226", st: aws.SweepRoleState{Exists: true, Schema: mine + 1, Principals: principals}, pass: true, saying: "newer spawn"},
		{name: "script's", acct: "435415984226", st: aws.SweepRoleState{Exists: true, Legacy: true, Principals: principals}, saying: "setup script"},
		{name: "older", acct: "435415984226", st: aws.SweepRoleState{Exists: true, Schema: mine, Legacy: true}, saying: "older"},
		{name: "absent", acct: "435415984226", saying: "not created yet"},
		{name: "unreadable", acct: "435415984226", err: errors.New("AccessDenied"), saying: "could not read"},
		{name: "forbidden, absent", acct: "942542972736", saying: "detached sweeps cannot run in this account"},
		{name: "forbidden, present", acct: "942542972736", st: aws.SweepRoleState{Exists: true}, fail: true, saying: "spawn sweep teardown"},
		{name: "management account, present", acct: "111122223333", st: aws.SweepRoleState{Exists: true}, fail: true,
			mgmt: func(context.Context) (string, error) { return "111122223333", nil }, saying: "management account"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgmt := c.mgmt
			if mgmt == nil {
				mgmt = none
			}
			detail, err := sweepRoleVerdict(t.Context(), c.acct, c.st, c.err, mgmt)
			if c.pass != (err == nil) {
				t.Fatalf("detail %q, err %v; want pass=%v", detail, err, c.pass)
			}
			if c.fail != errors.Is(err, ErrMustRemove) {
				t.Errorf("err %v; want a Fail=%v", err, c.fail)
			}
			if got := detail + fmt.Sprint(err); !strings.Contains(got, c.saying) {
				t.Errorf("%q does not say %q", got, c.saying)
			}
		})
	}
}

// The check is a Fail only for ErrMustRemove, a Warn otherwise.
func TestSweepRoleCheckSeverity(t *testing.T) {
	for _, c := range []struct {
		err  error
		want Status
	}{
		{nil, Pass},
		{errors.New("not created yet"), Warn},
		{fmt.Errorf("%w: in the forbidden account", ErrMustRemove), Fail},
	} {
		r := Run(context.Background(), mockProber{acct: "435415984226", sweepRoleErr: c.err})
		for _, ch := range r.Checks {
			if ch.Name == "sweep cross-account role" && ch.Status != c.want {
				t.Errorf("err %v: status %v, want %v", c.err, ch.Status, c.want)
			}
		}
	}
}
