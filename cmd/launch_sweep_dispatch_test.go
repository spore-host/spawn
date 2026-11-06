package cmd

import (
	"context"
	"errors"
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/sweep"
)

// The dispatch table (#1535): every combination of the detach flags and
// --max-concurrent, and the path and effective max-concurrent each takes.
// "--detach, no --max-concurrent" is the bug: it used to run in the
// foreground.
func TestSweepDispatchTable(t *testing.T) {
	type in struct {
		detach, noDetach bool
		maxConcurrent    int
		auto             bool
		rows             int
	}
	type want struct {
		path          sweepPath
		maxConcurrent int
		autoDetached  bool
		defaulted     bool
		deriveAuto    bool
	}
	cases := map[string]struct {
		in   in
		want want
	}{
		"no flag, 3 rows":                     {in{rows: 3}, want{sweepDetached, 3, true, true, false}},
		"no flag, 25 rows":                    {in{rows: 25}, want{sweepDetached, 10, true, true, false}},
		"no flag, --max-concurrent 4":         {in{maxConcurrent: 4, rows: 25}, want{sweepDetached, 4, true, false, false}},
		"no flag, --max-concurrent-auto":      {in{auto: true, rows: 25}, want{sweepDetached, 0, true, false, true}},
		"--detach, 3 rows":                    {in{detach: true, rows: 3}, want{sweepDetached, 3, false, true, false}},
		"--detach, 25 rows":                   {in{detach: true, rows: 25}, want{sweepDetached, 10, false, true, false}},
		"--detach --max-concurrent 1":         {in{detach: true, maxConcurrent: 1, rows: 25}, want{sweepDetached, 1, false, false, false}},
		"--detach --max-concurrent 50":        {in{detach: true, maxConcurrent: 50, rows: 25}, want{sweepDetached, 50, false, false, false}},
		"--detach --max-concurrent-auto":      {in{detach: true, auto: true, rows: 25}, want{sweepDetached, 0, false, false, true}},
		"--no-detach":                         {in{noDetach: true, rows: 25}, want{sweepForeground, 0, false, false, false}},
		"--no-detach --max-concurrent 4":      {in{noDetach: true, maxConcurrent: 4, rows: 25}, want{sweepForeground, 4, false, false, false}},
		"--no-detach --max-concurrent-auto":   {in{noDetach: true, auto: true, rows: 25}, want{sweepForeground, 0, false, false, true}},
		"--detach, 0 rows (never 0 detached)": {in{detach: true, rows: 0}, want{sweepDetached, 1, false, true, false}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := sweepDispatch(tc.in.detach, tc.in.noDetach, tc.in.maxConcurrent, tc.in.auto, tc.in.rows)
			if err != nil {
				t.Fatalf("sweepDispatch: %v", err)
			}
			w := sweepDispatchDecision{path: tc.want.path, maxConcurrent: tc.want.maxConcurrent,
				autoDetached: tc.want.autoDetached, defaulted: tc.want.defaulted, deriveAuto: tc.want.deriveAuto}
			if got != w {
				t.Errorf("sweepDispatch = %+v, want %+v", got, w)
			}
			if got.path == sweepDetached && !got.deriveAuto && got.maxConcurrent < 1 {
				t.Errorf("a detached sweep recorded with max-concurrent %d would launch nothing", got.maxConcurrent)
			}
		})
	}
	if d, _ := sweepDispatch(false, false, 0, true, 25); d.withDerived(7).maxConcurrent != 7 || d.withDerived(7).deriveAuto {
		t.Errorf("withDerived(7) = %+v, want max-concurrent 7, derived", d.withDerived(7))
	}
	for name, a := range map[string][4]int{
		"--detach --no-detach":                    {1, 1, 0, 0},
		"--max-concurrent with -auto":             {0, 0, 2, 1},
		"--max-concurrent negative, default path": {0, 0, -1, 0},
		"--max-concurrent negative, --no-detach":  {0, 1, -1, 0},
	} {
		if _, err := sweepDispatch(a[0] == 1, a[1] == 1, a[2], a[3] == 1, 3); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// roleIAMFake answers requireSweepCrossAccountRole's GetRole.
type roleIAMFake struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (f *roleIAMFake) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, awssdk.ToString(in.RoleName))
	if f.err != nil {
		return nil, f.err
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName}}, nil
}

func (f *roleIAMFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func stubSweepRoleIAM(t *testing.T, f *roleIAMFake) {
	t.Helper()
	prev := sweepRoleIAM
	t.Cleanup(func() { sweepRoleIAM = prev })
	sweepRoleIAM = func(awssdk.Config) sweep.RoleGetter { return f }
}

var errNoCrossAccountRole = &iamtypes.NoSuchEntityException{Message: awssdk.String("The role with name SpawnSweepCrossAccountRole cannot be found.")}

// dispatchSweepFile is a two-row sweep every orchestration path accepts.
func dispatchSweepFile(t *testing.T) string {
	t.Helper()
	pf := filepath.Join(t.TempDir(), "sweep.yaml")
	if err := os.WriteFile(pf, []byte("defaults:\n  instance_type: m7i.large\n  ttl: 1h\nparams:\n  - alpha: 1\n  - alpha: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return pf
}

// runDispatchSweep runs `spawn launch --param-file … <flags>` through
// runLaunch against the recording AWS fake, with the cross-account role
// absent, and returns its stderr, the fakes and its error.
func runDispatchSweep(t *testing.T, flags map[string]string) (string, *tagLimitAWS, *roleIAMFake, error) {
	t.Helper()
	f := serveTagLimitAWS(t)
	// The sweep writes audit records: keep ~/.spawn/audit.log in a temp dir
	// and close it, or Windows cannot remove the open file.
	withLogDir(t, false)
	t.Setenv("SPAWN_COMPUTE_PROFILE", "") // the fake's credentials, not spore-host-dev
	t.Setenv("SPAWN_INFRA_PROFILE", "")
	stubTeamName(t)
	role := &roleIAMFake{err: errNoCrossAccountRole}
	stubSweepRoleIAM(t, role)
	for k, v := range map[string]string{
		"param-file": dispatchSweepFile(t), "region": "us-east-1", "name": "disp", "yes": "true",
		"no-dns": "true", "ami": "ami-0123456789abcdef0", "os": "linux",
	} {
		setLaunchFlag(t, k, v)
	}
	for k, v := range flags {
		setLaunchFlag(t, k, v)
	}
	var err error
	out := captureStderr(t, func() { err = runLaunch(launchCmd, nil) })
	return out, f, role, err
}

func isRoleRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "a detached sweep needs SpawnSweepCrossAccountRole")
}

// The printed mode is the path the sweep takes (#1535), through the real
// runLaunch sweep path. The detached path is recognised by the one thing only
// it does: ask IAM for SpawnSweepCrossAccountRole (absent here, so it stops
// there). "--detach" is the bug: it printed "Lambda (detached)" and ran in
// the foreground.
func TestSweepPrintedModeIsTheDispatchedPath(t *testing.T) {
	cases := map[string]struct {
		flags         map[string]string
		path          sweepPath
		maxConcurrent string
	}{
		"no flag":                     {map[string]string{}, sweepDetached, "Max Concurrent: 2"},
		"--detach":                    {map[string]string{"detach": "true"}, sweepDetached, "Max Concurrent: 2"},
		"--detach --max-concurrent 1": {map[string]string{"detach": "true", "max-concurrent": "1"}, sweepDetached, "Max Concurrent: 1"},
		"--max-concurrent 1":          {map[string]string{"max-concurrent": "1"}, sweepDetached, "Max Concurrent: 1"},
		"--no-detach":                 {map[string]string{"no-detach": "true"}, sweepForeground, "Mode: All at once"},
		"--no-detach --max-concurrent 1": {map[string]string{"no-detach": "true", "max-concurrent": "1"},
			sweepForeground, "Max Concurrent: 1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, _, role, err := runDispatchSweep(t, tc.flags)
			took := sweepForeground
			if role.count() > 0 {
				took = sweepDetached
			}
			if took != tc.path {
				t.Fatalf("took the %s path, want %s (err %v)", took, tc.path, err)
			}
			if !strings.Contains(out, tc.maxConcurrent) {
				t.Errorf("output does not say %q:\n%s", tc.maxConcurrent, out)
			}
			printed := sweepForeground
			switch {
			case strings.Contains(out, "Orchestration: Lambda (detached)"):
				printed = sweepDetached
			case !strings.Contains(out, "Orchestration: foreground"):
				t.Fatalf("no Orchestration line:\n%s", out)
			}
			if printed != took {
				t.Errorf("printed %s orchestration, took the %s path:\n%s", printed, took, out)
			}
			if took == sweepDetached && !isRoleRefusal(err) {
				t.Errorf("detached sweep without the role: err = %v, want the cross-account role refusal", err)
			}
			if took == sweepForeground {
				if isRoleRefusal(err) {
					t.Errorf("a foreground sweep got the detached role refusal: %v", err)
				}
				if strings.Contains(out, "Lambda") {
					t.Errorf("a foreground sweep's output mentions Lambda:\n%s", out)
				}
			}
		})
	}
}

// A detached sweep in an account without SpawnSweepCrossAccountRole fails
// before it creates anything (#1535): the only AWS call past the identity
// lookups is the GetRole, so no IAM role, instance profile, key pair, egress
// proxy, params upload or orchestration record exists for a sweep the
// orchestrator could not launch. It used to get all the way to "queued
// successfully".
func TestDetachedSweepWithoutTheCrossAccountRoleCreatesNothing(t *testing.T) {
	for name, flags := range map[string]map[string]string{
		"default":                    {},
		"--detach":                   {"detach": "true"},
		"--detach with --iam-policy": {"detach": "true", "iam-policy": "s3:ReadOnly"},
	} {
		t.Run(name, func(t *testing.T) {
			out, f, role, err := runDispatchSweep(t, flags)
			if !isRoleRefusal(err) || !strings.Contains(err.Error(), "Nothing was created") || !strings.Contains(err.Error(), "--no-detach") {
				t.Fatalf("err = %v, want the cross-account role refusal", err)
			}
			if role.count() != 1 || role.calls[0] != sweepCrossAccountRoleName {
				t.Errorf("GetRole calls %v, want one for %s", role.calls, sweepCrossAccountRoleName)
			}
			if calls := f.beyondIdentity(); len(calls) != 0 {
				t.Errorf("the refused sweep made AWS call(s) beyond the identity lookup: %v", calls)
			}
			if strings.Contains(out, "Uploading parameters") || strings.Contains(out, "Creating sweep orchestration record") {
				t.Errorf("the refused sweep went on to submit:\n%s", out)
			}
		})
	}
}

// Only a definite absence refuses: a GetRole the caller may not make is
// reported and the sweep goes on, as before the check.
func TestCrossAccountRoleCheckRefusesOnlyAbsence(t *testing.T) {
	var warned []string
	warn := func(s string) { warned = append(warned, s) }
	if err := requireSweepCrossAccountRole(t.Context(), &roleIAMFake{}, "111122223333", warn); err != nil || len(warned) != 0 {
		t.Errorf("role present: err %v, warnings %v", err, warned)
	}
	if err := requireSweepCrossAccountRole(t.Context(), &roleIAMFake{err: errNoCrossAccountRole}, "111122223333", warn); !isRoleRefusal(err) || !strings.Contains(err.Error(), "111122223333") {
		t.Errorf("role absent: err %v, want the refusal naming the account", err)
	}
	denied := errors.New("AccessDenied: not authorized to perform iam:GetRole")
	if err := requireSweepCrossAccountRole(t.Context(), &roleIAMFake{err: denied}, "111122223333", warn); err != nil || len(warned) != 1 || !strings.Contains(warned[0], "could not confirm") {
		t.Errorf("GetRole denied: err %v, warnings %v; want a warning and no refusal", err, warned)
	}
}

// The detached-only refusals fire only on the detached path, and the
// foreground-only one only on the foreground path (#1535): a row naming the
// nested-spawn role gets the detached refusal (#1372) on every detached sweep,
// --detach without --max-concurrent included (which used to get the
// foreground TTL refusal), and the TTL refusal (#1375) with --no-detach.
func TestSweepRefusalsFollowTheDispatchedPath(t *testing.T) {
	cases := map[string]struct {
		flags    map[string]string
		detached bool
	}{
		"no flag":                     {map[string]string{}, true},
		"--detach":                    {map[string]string{"detach": "true"}, true},
		"--detach --max-concurrent 1": {map[string]string{"detach": "true", "max-concurrent": "1"}, true},
		"--no-detach":                 {map[string]string{"no-detach": "true"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			keepLaunchGlobals(t)
			p := filepath.Join(t.TempDir(), "sweep.json")
			body := `{"defaults": {"instance_type": "t3.micro", "region": "us-east-1", "idle_timeout": "1h"},
			          "params": [{"iam_role": "` + aws.NestedSpawnRoleName() + `"}]}`
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			setFlag(t, launchCmd, "param-file", p)
			setFlag(t, launchCmd, "ttl", "")
			for k, v := range tc.flags {
				setFlag(t, launchCmd, k, v)
			}
			calls := 0
			t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls))
			err := launchParameterSweep(context.Background(), &aws.LaunchConfig{InstanceType: "t3.micro", Region: "us-east-1"}, nil, nil)
			detachedRefusal := err != nil && strings.Contains(err.Error(), "a detached sweep cannot use")
			if detachedRefusal != tc.detached || errors.Is(err, aws.ErrNestedSpawnNeedsTTL) == tc.detached {
				t.Fatalf("err = %v; want the %s refusal", err, map[bool]string{true: "detached-sweep", false: "nested-spawn TTL"}[tc.detached])
			}
			if calls != 0 {
				t.Errorf("the refused sweep made %d AWS call(s) first", calls)
			}
		})
	}
}

// --wait follows the path too: it is accepted on every detached sweep,
// including the default one it used to refuse, and refused with --no-detach,
// where there is no orchestration record to wait on.
func TestSweepWaitFollowsTheDispatchedPath(t *testing.T) {
	for name, tc := range map[string]struct {
		flags   map[string]string
		refused bool
	}{
		"--wait (default detach)": {map[string]string{"wait": "true"}, false},
		"--wait --detach":         {map[string]string{"wait": "true", "detach": "true"}, false},
		"--wait --no-detach":      {map[string]string{"wait": "true", "no-detach": "true"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, role, err := runDispatchSweep(t, tc.flags)
			waitRefused := err != nil && strings.Contains(err.Error(), "--wait waits for a detached sweep")
			if waitRefused != tc.refused {
				t.Fatalf("err = %v; want --wait refused: %v", err, tc.refused)
			}
			if !tc.refused && !isRoleRefusal(err) {
				t.Errorf("err = %v; want the sweep to reach the detached path (the role check)", err)
			}
			if tc.refused && role.count() != 0 {
				t.Errorf("a refused --wait sweep asked IAM for the role first")
			}
		})
	}
}

// Gate (#1535): the --detach, --no-detach, --max-concurrent and
// --max-concurrent-auto globals are read only as arguments to sweepDispatch
// (and bound in launch_flags.go). Any other reader re-derives the path, which
// is how `detach && maxConcurrent > 0` came to disagree with the summary line.
func TestSweepDispatchIsTheOnlyReaderOfTheDetachFlags(t *testing.T) {
	fset, files, info := typeCheckCmdPackage(t)
	watched := map[string]bool{"detach": true, "noDetach": true, "maxConcurrent": true, "maxConcurrentAuto": true}
	asArg := 0
	for _, f := range files {
		file := filepath.Base(fset.Position(f.Pos()).Filename)
		if file == "launch_flags.go" {
			continue
		}
		args := map[*ast.Ident]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if fn, ok := c.Fun.(*ast.Ident); ok && fn.Name == "sweepDispatch" {
					for _, a := range c.Args {
						if id, ok := a.(*ast.Ident); ok {
							args[id] = true
						}
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !watched[id.Name] {
				return true
			}
			obj := info.Uses[id]
			if obj == nil || obj.Parent() == nil || obj.Parent() != obj.Pkg().Scope() {
				return true // a local, a parameter, a field or a declaration
			}
			if args[id] {
				asArg++
				return true
			}
			t.Errorf("%s: reads the --%s global directly; take the path from sweepDispatch's decision", fset.Position(id.Pos()), id.Name)
			return true
		})
	}
	// A discovery that found nothing has broken, not proven the code clean.
	if asArg < 8 {
		t.Fatalf("found %d sweepDispatch arguments naming the flags, want at least 8 (two calls); the discovery is broken", asArg)
	}
}
