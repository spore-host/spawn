package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/spore-host/spawn/pkg/aws"
)

// #1537: a detached sweep provisions SpawnSweepCrossAccountRole, and `spawn
// sweep setup` / `spawn sweep teardown` create and delete it on their own.

// A detached sweep in an account without the role creates it, and creating
// it is the first thing the sweep does in AWS: when CreateRole runs, nothing
// but the identity lookups has reached the account. It then writes the
// versioned policy and goes on (the fake stops it later).
func TestDetachedSweepCreatesTheRoleFirst(t *testing.T) {
	f := serveTagLimitAWS(t)
	withLogDir(t, false)
	t.Setenv("SPAWN_COMPUTE_PROFILE", "")
	t.Setenv("SPAWN_INFRA_PROFILE", "")
	stubTeamName(t)
	var before []string
	role := &roleIAMFake{err: errNoCrossAccountRole}
	role.onCreate = func() { before = f.beyondIdentity() }
	stubSweepRoleIAM(t, role)
	for k, v := range map[string]string{
		"param-file": dispatchSweepFile(t), "region": "us-east-1", "name": "mk", "yes": "true",
		"no-dns": "true", "ami": "ami-0123456789abcdef0", "os": "linux", "detach": "true",
	} {
		setLaunchFlag(t, k, v)
	}
	var err error
	out := captureStderr(t, func() { err = runLaunch(launchCmd, nil) })

	if isRoleRefusal(err) {
		t.Fatalf("the sweep was refused although the role could be created: %v", err)
	}
	if !role.created {
		t.Fatalf("the role was not created; IAM calls %v", role.allOps())
	}
	if len(before) != 0 {
		t.Errorf("the sweep made AWS calls before creating the role: %v", before)
	}
	if _, ok := role.policies[aws.SweepRolePolicyName()]; !ok {
		t.Errorf("no %s written; IAM calls %v", aws.SweepRolePolicyName(), role.allOps())
	}
	if !strings.Contains(out, "Created SpawnSweepCrossAccountRole in account 123456789012") ||
		!strings.Contains(out, "spawn sweep teardown") {
		t.Errorf("stderr does not report the role it created and its deleter:\n%s", out)
	}
}

// ensureSweepRoleForTest runs ensureSweepRole for account with role as its
// IAM, and returns stderr, stdout and the error.
func ensureSweepRoleForTest(t *testing.T, role *roleIAMFake, account string, strict bool) (string, string, error) {
	t.Helper()
	stubSweepRoleIAM(t, role)
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = ensureSweepRole(t.Context(), awssdk.Config{}, account, strict, &out) })
	return stderr, out.String(), err
}

// The Amazon-internal management account is refused as the compute account
// and as the orchestrator's, on the detached path and by `spawn sweep setup`,
// before any IAM call and before Organizations is asked.
func TestSweepRoleRefusesTheForbiddenAccount(t *testing.T) {
	const forbidden = "942542972736"
	for _, strict := range []bool{false, true} {
		for _, orchestratorIsForbidden := range []bool{false, true} {
			role := &roleIAMFake{err: errNoCrossAccountRole}
			compute := forbidden
			if orchestratorIsForbidden {
				compute = "111122223333"
			}
			stubSweepRoleIAM(t, role)
			if orchestratorIsForbidden {
				sweepOrchestratorAccount = func(context.Context) (string, error) { return forbidden, nil }
			}
			asked := false
			sweepRoleManagementAccount = func(context.Context, awssdk.Config) (string, error) { asked = true; return "", nil }

			var out bytes.Buffer
			err := ensureSweepRole(t.Context(), awssdk.Config{}, compute, strict, &out)
			if err == nil {
				t.Fatalf("strict %v, orchestrator forbidden %v: no refusal", strict, orchestratorIsForbidden)
			}
			for _, want := range []string{forbidden, "Amazon-internal", "o-pygqyjjoym", "refusing"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if !strict && (!isRoleRefusal(err) || !strings.Contains(err.Error(), "Nothing was created")) {
				t.Errorf("the detached refusal is not the sweep's: %v", err)
			}
			if ops := role.allOps(); len(ops) != 0 {
				t.Errorf("IAM calls in a refused account: %v", ops)
			}
			if asked {
				t.Error("Organizations was asked about an account refused by name")
			}
		}
	}
}

// An Organizations management account is refused too, as the compute
// account, before any IAM call.
func TestSweepRoleRefusesAManagementAccount(t *testing.T) {
	role := &roleIAMFake{err: errNoCrossAccountRole}
	stubSweepRoleIAM(t, role)
	sweepRoleManagementAccount = func(context.Context, awssdk.Config) (string, error) { return "111122223333", nil }
	var out bytes.Buffer
	err := ensureSweepRole(t.Context(), awssdk.Config{}, "111122223333", false, &out)
	if !isRoleRefusal(err) || !strings.Contains(err.Error(), "management account") {
		t.Fatalf("err = %v, want the management-account refusal", err)
	}
	if ops := role.allOps(); len(ops) != 0 {
		t.Errorf("IAM calls in a management account: %v", ops)
	}

	// Unreadable Organizations: a warning, and the role is ensured.
	role = &roleIAMFake{}
	stubSweepRoleIAM(t, role)
	sweepRoleManagementAccount = func(context.Context, awssdk.Config) (string, error) {
		return "", errors.New("AccessDeniedException")
	}
	stderr := captureStderr(t, func() { err = ensureSweepRole(t.Context(), awssdk.Config{}, "111122223333", false, &out) })
	if err != nil || !strings.Contains(stderr, "could not tell whether account 111122223333 is an AWS Organizations management account") {
		t.Errorf("unreadable organization: err %v, stderr %q", err, stderr)
	}
}

// The detached path refuses only when the sweep could launch nothing: no
// role, and none could be made. A role it may not look at or update is the
// role the sweep used before #1537, and the sweep goes on with a warning.
// `spawn sweep setup` (strict) reports every failure.
func TestEnsureSweepRoleOutcomes(t *testing.T) {
	denied := errors.New("AccessDenied: not authorized")
	cases := []struct {
		name       string
		role       func() *roleIAMFake
		refused    bool   // the detached path refuses
		warn       string // the detached path warns
		out        string
		strictFail bool
	}{
		{name: "present", role: func() *roleIAMFake { return &roleIAMFake{} }},
		{name: "absent, created", role: func() *roleIAMFake { return &roleIAMFake{err: errNoCrossAccountRole} }, out: "Created SpawnSweepCrossAccountRole"},
		{name: "absent, may not create", role: func() *roleIAMFake { return &roleIAMFake{err: errNoCrossAccountRole, createErr: denied} }, refused: true, strictFail: true},
		{name: "may not look", role: func() *roleIAMFake { return &roleIAMFake{err: denied} }, warn: "could not confirm", strictFail: true},
		{name: "present, may not update", role: func() *roleIAMFake { return &roleIAMFake{putErr: denied} }, warn: "could not bring it up to date", strictFail: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stderr, out, err := ensureSweepRoleForTest(t, c.role(), "111122223333", false)
			if c.refused != isRoleRefusal(err) || (!c.refused && err != nil) {
				t.Errorf("detached: err %v, want refused=%v", err, c.refused)
			}
			if c.refused && !strings.Contains(err.Error(), "Nothing was created") {
				t.Errorf("the refusal does not say nothing was created: %v", err)
			}
			if c.warn != "" && !strings.Contains(stderr, c.warn) {
				t.Errorf("stderr %q does not warn %q", stderr, c.warn)
			}
			if c.out != "" && !strings.Contains(out, c.out) {
				t.Errorf("output %q does not say %q", out, c.out)
			}

			_, _, err = ensureSweepRoleForTest(t, c.role(), "111122223333", true)
			if (err != nil) != c.strictFail {
				t.Errorf("strict: err %v, want failure=%v", err, c.strictFail)
			}
		})
	}

	// The orchestrator's account cannot be resolved: there is nothing to trust.
	role := &roleIAMFake{err: errNoCrossAccountRole}
	stubSweepRoleIAM(t, role)
	sweepOrchestratorAccount = func(context.Context) (string, error) { return "", errors.New("no infra profile") }
	var out bytes.Buffer
	if err := ensureSweepRole(t.Context(), awssdk.Config{}, "111122223333", false, &out); !isRoleRefusal(err) || len(role.allOps()) != 0 {
		t.Errorf("unresolvable orchestrator: err %v, IAM calls %v", err, role.allOps())
	}
}

// stubActiveSweeps answers teardown's question about running sweeps.
func stubActiveSweeps(t *testing.T, ids []string, err error) {
	t.Helper()
	prev := activeSweepsInAccount
	t.Cleanup(func() { activeSweepsInAccount = prev })
	activeSweepsInAccount = func(context.Context, string) ([]string, error) { return ids, err }
}

func hasOp(ops []string, op string) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

// Teardown refuses while a sweep in the account runs, or when it cannot tell,
// unless --force; otherwise it deletes the role's policies and then the role.
func TestSweepTeardown(t *testing.T) {
	present := func() *roleIAMFake {
		return &roleIAMFake{policies: map[string]string{aws.SweepRolePolicyName(): "{}", "SweepCrossAccountEC2Policy": "{}"}}
	}
	run := func(role *roleIAMFake, force bool) (string, string, error) {
		stubSweepRoleIAM(t, role)
		var out bytes.Buffer
		var err error
		stderr := captureStderr(t, func() { err = runSweepTeardown(t.Context(), awssdk.Config{}, "111122223333", force, &out) })
		return out.String(), stderr, err
	}

	// No role: nothing to do, and running sweeps are not asked about.
	stubActiveSweeps(t, nil, errors.New("must not be asked"))
	role := &roleIAMFake{err: errNoCrossAccountRole}
	if out, _, err := run(role, false); err != nil || !strings.Contains(out, "nothing to remove") || hasOp(role.allOps(), "DeleteRole") {
		t.Errorf("no role: err %v, out %q, ops %v", err, out, role.allOps())
	}

	// A running sweep: refused, naming it.
	stubActiveSweeps(t, []string{"sweep-1 (RUNNING)"}, nil)
	role = present()
	_, _, err := run(role, false)
	if err == nil || !strings.Contains(err.Error(), "sweep-1 (RUNNING)") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("running sweep: err %v, want a refusal naming it", err)
	}
	if hasOp(role.allOps(), "DeleteRole") || len(role.policies) != 2 {
		t.Errorf("the refused teardown deleted something: %v", role.allOps())
	}

	// The table cannot be read: refused.
	stubActiveSweeps(t, nil, errors.New("AccessDeniedException: dynamodb:Scan"))
	role = present()
	if _, _, err := run(role, false); err == nil || !strings.Contains(err.Error(), "could not check for running sweeps") || hasOp(role.allOps(), "DeleteRole") {
		t.Errorf("unreadable table: err %v, ops %v", err, role.allOps())
	}

	// --force: deleted all the same, with a warning.
	stubActiveSweeps(t, []string{"sweep-1 (RUNNING)"}, nil)
	role = present()
	out, stderr, err := run(role, true)
	if err != nil || !hasOp(role.allOps(), "DeleteRole") || !strings.Contains(stderr, "--force") || !strings.Contains(out, "Deleted") {
		t.Errorf("--force: err %v, ops %v, stderr %q", err, role.allOps(), stderr)
	}

	// Nothing running: the policies, then the role.
	stubActiveSweeps(t, nil, nil)
	role = present()
	if _, _, err := run(role, false); err != nil {
		t.Fatal(err)
	}
	ops := role.allOps()
	if len(role.policies) != 0 || ops[len(ops)-1] != "DeleteRole" {
		t.Errorf("teardown ops %v, policies left %v; want every policy deleted, then the role", ops, role.policies)
	}
}

// `spawn sweep setup` and `spawn sweep teardown` exist, under `spawn sweep`.
func TestSweepSetupAndTeardownAreCommands(t *testing.T) {
	for _, name := range []string{"setup", "teardown"} {
		c, _, err := rootCmd.Find([]string{"sweep", name})
		if err != nil || c.Name() != name {
			t.Errorf("spawn sweep %s: %v", name, err)
		}
	}
	if sweepTeardownCmd.Flags().Lookup("force") == nil {
		t.Error("spawn sweep teardown has no --force")
	}
}

// roleIAMFake must stay an aws.SweepRoleIAM.
var _ aws.SweepRoleIAM = (*roleIAMFake)(nil)
