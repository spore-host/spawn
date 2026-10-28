package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// #1385: a resume reused the spawn-instance-* profile a sweep was launched with
// without re-creating it, so once the idle-role sweep deleted the role every
// resumed member failed at RunInstances. And the checkpoint never recorded the
// CLI IAM flags' profile at all, so a resumed member silently got
// spored-instance-profile instead.

// fakeProfileIAM is resumeProfileEnsurer over a set of existing profiles. Every
// call goes into log, which the launcher below shares, so a test sees the order
// of IAM calls and launches.
type fakeProfileIAM struct {
	exists  map[string]bool
	log     *[]string
	created []aws.IAMRoleConfig
	nested  int
}

func (f *fakeProfileIAM) record(s string) {
	if f.log != nil {
		*f.log = append(*f.log, s)
	}
}

func (f *fakeProfileIAM) InstanceProfileReady(_ context.Context, name string) (bool, error) {
	f.record("ready " + name)
	return f.exists[name], nil
}

func (f *fakeProfileIAM) CreateOrGetInstanceProfile(_ context.Context, cfg aws.IAMRoleConfig) (string, error) {
	name := cfg.RoleName
	if name == "" {
		name = aws.PredictedInstanceRoleName(cfg)
	}
	f.record("create " + name)
	f.created = append(f.created, cfg)
	if f.exists == nil {
		f.exists = map[string]bool{}
	}
	f.exists[name] = true
	return name, nil
}

func (f *fakeProfileIAM) SetupNestedSpawnProfile(context.Context) (string, error) {
	f.nested++
	f.record("nested " + aws.NestedSpawnRoleName())
	return aws.NestedSpawnRoleName(), nil
}

// loggingLauncher is fakeSweepLauncher writing each launch, with its profile,
// into the shared log.
type loggingLauncher struct {
	fakeSweepLauncher
	log *[]string
}

func (l *loggingLauncher) Launch(ctx context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	*l.log = append(*l.log, "launch "+cfg.IamInstanceProfile)
	return l.fakeSweepLauncher.Launch(ctx, cfg)
}

// The CLI IAM config a sweep launched with: --iam-policy s3:ReadOnly plus
// --s3-read, i.e. a policy list and a scoped inline policy.
func originalSweepIAM() aws.IAMRoleConfig {
	return aws.IAMRoleConfig{
		Policies:         []string{"s3:ReadOnly"},
		InlinePolicyJSON: taskStagingPolicy([]string{"my-data"}, nil, "", nil),
		TrustServices:    []string{"ec2"},
		Tags:             map[string]string{"project": "x"},
	}
}

// resumeFixture builds the pending configs a resume launches, from a param file
// with no iam_role: of its own and the checkpoint, the way runResume does.
func resumeFixture(t *testing.T, state *SweepState, pending []int) []*aws.LaunchConfig {
	t.Helper()
	pf := &ParamFileFormat{Defaults: map[string]interface{}{"instance_type": "t3.micro", "region": "us-west-2"}}
	for i := 0; i < state.TotalParams; i++ {
		pf.Params = append(pf.Params, map[string]interface{}{"ttl": "1h"})
	}
	applyRecordedIAM(state.IAMRole, pf)
	var out []*aws.LaunchConfig
	for _, idx := range pending {
		cfg, err := buildLaunchConfigFromParams(aws.LaunchConfig{}, pf.Defaults, pf.Params[idx], state.SweepID, state.SweepName, idx, state.TotalParams)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, &cfg)
	}
	return out
}

func newRecordedState(t *testing.T, rec *SweepIAMRole) *SweepState {
	t.Helper()
	return &SweepState{
		SweepID: "sweep-1385", SweepName: "s", CreatedAt: time.Now(), ParamFile: "/data/params.yaml",
		TotalParams: 2, MaxConcurrent: 1, LaunchDelay: "0s", IAMRecorded: true, IAMRole: rec,
	}
}

// Gate 1: the role was deleted; the resume re-creates it with the original
// policy, under the same name, before the first RunInstances, and the members
// launch with it.
func TestResume_RecreatesDeletedSpawnRoleWithOriginalPolicyBeforeLaunch(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)

	orig := originalSweepIAM()
	profile := aws.PredictedInstanceRoleName(orig)
	rec, err := newSweepIAMRole(profile, orig, "--iam-policy s3:ReadOnly --s3-read my-data")
	if err != nil {
		t.Fatal(err)
	}
	// Through the checkpoint file, as a real resume reads it.
	if err := saveSweepState(newRecordedState(t, rec)); err != nil {
		t.Fatal(err)
	}
	state, err := loadSweepState("sweep-1385")
	if err != nil {
		t.Fatal(err)
	}

	configs := resumeFixture(t, state, []int{0, 1})
	var log []string
	iam := &fakeProfileIAM{exists: map[string]bool{}, log: &log}
	l := &loggingLauncher{log: &log}
	if _, _, n, err := relaunchPending(context.Background(), l, state, configs, []int{0, 1}, 1, unverifiedGuard(), iam); err != nil || n != 2 {
		t.Fatalf("relaunchPending: n=%d err=%v", n, err)
	}

	want := []string{"ready " + profile, "create " + profile, "launch " + profile, "launch " + profile}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("calls = %q\nwant  %q (re-created once, before the first launch, and launched with)", log, want)
	}
	if len(iam.created) != 1 || !reflect.DeepEqual(iam.created[0], orig) {
		t.Fatalf("re-created with %+v\nwant the original config %+v", iam.created, orig)
	}
}

// Gate 1, the record half: a sweep's checkpoint carries the CLI IAM profile and
// its config, so the resume above has something to re-create from.
func TestSweepCheckpointRecordsCLIIAMConfig(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)

	orig := originalSweepIAM()
	rec, err := newSweepIAMRole(aws.PredictedInstanceRoleName(orig), orig, "--iam-policy s3:ReadOnly")
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeFleetSweep(0)
	g := newFleetGuard("sweep", io.Discard, f, sporedUp(), false)
	if err := runSweepLaunch(context.Background(), f, g, sweepRun{
		sweepID: "rec", name: "rec", totalParams: 3, maxConcurrent: 1, launchDelay: "0s",
		configs: sweepConfigs("us-east-1", "us-east-1", "us-east-1"), iam: rec,
	}); err != nil {
		t.Fatal(err)
	}
	saved, err := loadSweepState("rec")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.IAMRecorded || saved.IAMRole == nil || saved.IAMRole.Config == nil {
		t.Fatalf("checkpoint IAM = recorded:%v %+v, want the CLI profile and its config", saved.IAMRecorded, saved.IAMRole)
	}
	if got := saved.IAMRole.Config.roleConfig(); !reflect.DeepEqual(got, orig) || saved.IAMRole.Profile != rec.Profile {
		t.Fatalf("checkpoint config = %+v (%s), want %+v (%s)", got, saved.IAMRole.Profile, orig, rec.Profile)
	}
}

// Gate 2: a checkpoint written before #1385 records no IAM config. A
// spawn-instance-* profile its param file names is gone: the resume is refused
// before any IAM write or launch, and says which flags it could not recover.
func TestResume_OldCheckpointWithRoleGoneIsRefusedWithZeroLaunches(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)

	state := &SweepState{SweepID: "old", SweepName: "s", ParamFile: "/data/params.yaml", TotalParams: 2, MaxConcurrent: 1, LaunchDelay: "0s"}
	const gone = "spawn-instance-0badc0de"
	configs := []*aws.LaunchConfig{
		{Name: "s-0", Region: "us-west-2", IamInstanceProfile: gone, TTL: "1h"},
		{Name: "s-1", Region: "us-west-2", IamInstanceProfile: gone, TTL: "1h"},
	}
	var log []string
	iam := &fakeProfileIAM{exists: map[string]bool{}, log: &log}
	l := &loggingLauncher{log: &log}
	_, _, n, err := relaunchPending(context.Background(), l, state, configs, []int{0, 1}, 1, unverifiedGuard(), iam)
	var pe *resumeProfileError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want the resume refused before launching", err)
	}
	for _, s := range []string{gone, "--iam-policy", "--s3-read", "Nothing was launched"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("refusal %q does not mention %q", err, s)
		}
	}
	if n != 0 || len(l.launched) != 0 || len(iam.created) != 0 || iam.nested != 0 {
		t.Fatalf("launched %d (%v), created %v, nested %d; want nothing", n, l.launched, iam.created, iam.nested)
	}
	if want := []string{"ready " + gone}; !reflect.DeepEqual(log, want) {
		t.Fatalf("calls = %q, want only %q", log, want)
	}
}

// Gate 2, the silent half: an old checkpoint loses the CLI profile entirely,
// so a resumed row would get spored-instance-profile. When the sweep's own
// instances show a different profile, the resume is refused.
func TestResume_OldCheckpointRefusesADifferentRole(t *testing.T) {
	pf := &ParamFileFormat{Params: []map[string]interface{}{{}, {}, {"iam_role": "own-role"}}}
	old := &SweepState{ParamFile: "/data/params.yaml", TotalParams: 3}
	err := refuseOldCheckpointRoleDrift(old, pf, map[int]string{0: "spawn-instance-1234abcd"})
	if err == nil || !strings.Contains(err.Error(), "spawn-instance-1234abcd") || !strings.Contains(err.Error(), aws.SporedInstanceProfileName) {
		t.Fatalf("err = %v, want a refusal naming both profiles", err)
	}
	// Same profile as the resume would use, a row with its own iam_role:, and
	// a checkpoint that records its IAM: not refused.
	for name, c := range map[string]struct {
		st    *SweepState
		profs map[int]string
	}{
		"same":     {old, map[int]string{0: aws.SporedInstanceProfileName}},
		"own role": {old, map[int]string{2: "spawn-instance-1234abcd"}},
		"recorded": {&SweepState{IAMRecorded: true, TotalParams: 3}, map[int]string{0: "spawn-instance-1234abcd"}},
	} {
		if err := refuseOldCheckpointRoleDrift(c.st, pf, c.profs); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// A recorded --iam-policy-file that changed since the launch cannot rebuild the
// same policy under the same name: refused, nothing created.
func TestResume_ChangedPolicyFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	pfPath := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(pfPath, []byte(`{"Version":"2012-10-17","Statement":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := aws.IAMRoleConfig{PolicyFile: pfPath}
	profile := aws.PredictedInstanceRoleName(cfg)
	rec, err := newSweepIAMRole(profile, cfg, "--iam-policy-file "+pfPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pfPath, []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	iam := &fakeProfileIAM{exists: map[string]bool{}}
	err = ensureResumeProfiles(context.Background(), iam, rec, "p", []*aws.LaunchConfig{{IamInstanceProfile: profile}})
	if err == nil || !strings.Contains(err.Error(), pfPath) || len(iam.created) != 0 {
		t.Fatalf("err = %v, created %v; want a refusal naming the file and nothing created", err, iam.created)
	}
}

// Gate 3: a profile that still exists is reused unchanged: only the read-only
// readiness check, no create or rewrite, and the members launch with it.
func TestResume_ExistingRoleReusedUnchanged(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)

	orig := originalSweepIAM()
	profile := aws.PredictedInstanceRoleName(orig)
	rec, err := newSweepIAMRole(profile, orig, "--iam-policy s3:ReadOnly")
	if err != nil {
		t.Fatal(err)
	}
	state := newRecordedState(t, rec)
	configs := resumeFixture(t, state, []int{0, 1})
	var log []string
	iam := &fakeProfileIAM{exists: map[string]bool{profile: true}, log: &log}
	l := &loggingLauncher{log: &log}
	if _, _, n, err := relaunchPending(context.Background(), l, state, configs, []int{0, 1}, 1, unverifiedGuard(), iam); err != nil || n != 2 {
		t.Fatalf("relaunchPending: n=%d err=%v", n, err)
	}
	want := []string{"ready " + profile, "launch " + profile, "launch " + profile}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("calls = %q, want %q (no IAM write)", log, want)
	}
}

// A gone --nested-spawn role has fixed grants, so it is re-created exactly.
func TestResume_RecreatesNestedSpawnRole(t *testing.T) {
	name := aws.NestedSpawnRoleName()
	iam := &fakeProfileIAM{exists: map[string]bool{}}
	if err := ensureResumeProfiles(context.Background(), iam, nil, "p", []*aws.LaunchConfig{{IamInstanceProfile: name}}); err != nil {
		t.Fatal(err)
	}
	if iam.nested != 1 || len(iam.created) != 0 {
		t.Fatalf("nested=%d created=%v, want SetupNestedSpawnProfile once", iam.nested, iam.created)
	}
}

// Detached resume: the record launchSweepDetached writes is what
// ensureDetachedResumeProfile re-creates from.
func TestDetachedResume_RecreatesRecordedRole(t *testing.T) {
	setTestHome(t, t.TempDir())
	orig := originalSweepIAM()
	profile := aws.PredictedInstanceRoleName(orig)
	rec, err := newSweepIAMRole(profile, orig, "--iam-policy s3:ReadOnly")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveDetachedIAMRecord("det", rec); err != nil {
		t.Fatal(err)
	}
	iam := &fakeProfileIAM{exists: map[string]bool{}}
	client := func(context.Context) (resumeProfileEnsurer, error) { return iam, nil }
	if err := ensureDetachedResumeProfile(context.Background(), "det", client); err != nil {
		t.Fatal(err)
	}
	if len(iam.created) != 1 || !reflect.DeepEqual(iam.created[0], orig) {
		t.Fatalf("created %+v, want the original config", iam.created)
	}
	// No record: nothing is called.
	calls := 0
	none := func(context.Context) (resumeProfileEnsurer, error) { calls++; return iam, nil }
	if err := ensureDetachedResumeProfile(context.Background(), "unrecorded", none); err != nil || calls != 0 {
		t.Fatalf("err=%v calls=%d for a sweep with no record", err, calls)
	}
}

// The resume paths reach the mechanism: runResume restores the recorded
// profile and runs the old-checkpoint check, resumeSweepDetached re-ensures
// before invoking the orchestrator, and launchSweepDetached writes the record
// before the upload.
func TestResumePathsReachTheIAMRecord(t *testing.T) {
	fset := token.NewFileSet()
	calls := map[string][]string{} // func -> callee names in source order
	for _, file := range []string{"resume.go", "launch_sweep.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					switch fn := c.Fun.(type) {
					case *ast.Ident:
						calls[fd.Name.Name] = append(calls[fd.Name.Name], fn.Name)
					case *ast.SelectorExpr:
						calls[fd.Name.Name] = append(calls[fd.Name.Name], fn.Sel.Name)
					}
				}
				return true
			})
		}
	}
	before := func(fn, first, then string) {
		t.Helper()
		i, j := -1, -1
		for k, c := range calls[fn] {
			if c == first && i < 0 {
				i = k
			}
			if c == then && j < 0 {
				j = k
			}
		}
		if i < 0 || j < 0 || i > j {
			t.Errorf("%s: want a call to %s before %s (%d, %d)", fn, first, then, i, j)
		}
	}
	before("runResume", "applyRecordedIAM", "refuseNestedSweepRowsWithoutTTL")
	before("runResume", "refuseOldCheckpointRoleDrift", "relaunchPending")
	before("relaunchPending", "ensureResumeProfiles", "launchWithRollingQueue")
	before("resumeSweepDetached", "ensureDetachedResumeProfile", "InvokeSweepOrchestrator")
	before("launchSweepDetached", "saveDetachedIAMRecord", "UploadParamsToS3")
}
