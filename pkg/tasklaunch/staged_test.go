package tasklaunch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskiam"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// These are the gates for #1274's second half in the task launcher: a release
// build stages the task wrapper and flush hook in S3, so a task's user data no
// longer grows with its command and manifests.

// asRelease makes this test binary a release build for the test, as
// `spawn task run` from a tagged release is: the bootstrap body is fetched
// and the task's files are staged.
func asRelease(t *testing.T) {
	t.Helper()
	prev := launcher.CallerVersion
	t.Cleanup(func() { launcher.CallerVersion = prev })
	launcher.CallerVersion = "0.143.0"
	if launcher.PublishedBodyVersion() == "" {
		t.Fatal("a release CallerVersion did not make a release build (no signing key compiled in?)")
	}
}

// asDev makes it a dev build: no release body to fetch, everything inline.
func asDev(t *testing.T) {
	t.Helper()
	prev := launcher.CallerVersion
	t.Cleanup(func() { launcher.CallerVersion = prev })
	launcher.CallerVersion = "dev"
}

// stagingAWS is fakeAWS with the S3 staging calls, recording the order of
// every object call.
type stagingAWS struct {
	fakeAWS
	puts      map[string][]byte // bucket/key -> body
	listed    []string          // prefixes listed
	existed   []string          // keys ListS3Keys returns
	putErr    error
	ensureErr error
	owners    []string // expected-owner of every staging-bucket call
	calls     []string
}

func (s *stagingAWS) EnsureStagingBucket(_ context.Context, bucket, _, owner string) error {
	s.calls = append(s.calls, "ensure "+bucket)
	s.owners = append(s.owners, owner)
	return s.ensureErr
}

func (s *stagingAWS) PutS3Object(_ context.Context, region, bucket, key, owner string, body []byte) error {
	s.calls = append(s.calls, "put "+key)
	s.owners = append(s.owners, owner)
	if s.putErr != nil {
		return s.putErr
	}
	if s.puts == nil {
		s.puts = map[string][]byte{}
	}
	s.puts[bucket+"/"+key] = append([]byte{}, body...)
	return nil
}

func (s *stagingAWS) ListS3Keys(_ context.Context, _, bucket, prefix, owner string) ([]string, error) {
	s.calls = append(s.calls, "list "+bucket+"/"+prefix)
	s.owners = append(s.owners, owner)
	s.listed = append(s.listed, prefix)
	return s.existed, nil
}

func (s *stagingAWS) DeleteS3Object(ctx context.Context, region, bucket, key string) error {
	s.calls = append(s.calls, "delete "+key)
	return s.fakeAWS.DeleteS3Object(ctx, region, bucket, key)
}

// recordingProvisioner records what it is asked to launch, and the object
// calls made before it, and launches.
type recordingProvisioner struct {
	cfg        aws.LaunchConfig
	opts       launcher.Options
	n          int
	callsAtRun []string
	store      *stagingAWS
}

func (p *recordingProvisioner) Provision(_ context.Context, cfg aws.LaunchConfig, opts launcher.Options) (*aws.LaunchResult, error) {
	p.cfg, p.opts, p.n = cfg, opts, p.n+1
	if p.store != nil {
		p.callsAtRun = append([]string{}, p.store.calls...)
	}
	return &aws.LaunchResult{InstanceID: "i-0staged", Region: cfg.Region}, nil
}

func userDataOf(t *testing.T, cfg aws.LaunchConfig, opts launcher.Options) (decoded int, script string) {
	t.Helper()
	enc, err := launcher.BuildUserData(cfg, opts)
	if err != nil {
		t.Fatalf("BuildUserData: %v", err)
	}
	if err := aws.CheckUserDataSize(enc); err != nil {
		t.Fatalf("the user data the launch sends is over EC2's limit: %v", err)
	}
	n, _ := aws.UserDataSize(enc)
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	text, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return n, string(text)
}

// largeCommandBytes is a task command that is far over what fits in the user
// data inline: 20 KB of incompressible text.
const largeCommandBytes = 20 * 1024

// testStagingBucket is the staging bucket of fakeAWS's account in us-east-1.
const testStagingBucket = "spawn-staging-123456789012-us-east-1"

// TestLargeTaskLaunchesWithinBudgetWhenStaged is the #1274 gate. A 20 KB
// inline command does not fit in the user data from a dev build (inline, as
// every task was before), and from a release build launches: its dry run
// passes and says the files are staged, Launch uploads them before
// RunInstances, and the user data actually sent is within EC2's limit and
// carries only the reference.
func TestLargeTaskLaunchesWithinBudgetWhenStaged(t *testing.T) {
	spec := userDataSpec(largeCommandBytes)

	t.Run("inline, from a dev build, it does not fit", func(t *testing.T) {
		asDev(t)
		if err := renderDryRun(context.Background(), &bytes.Buffer{}, spec, userDataTestFinder(), "us-east-1"); err == nil || !strings.Contains(err.Error(), "16384") {
			t.Fatalf("dry run = %v; the fixture should be over the limit inline", err)
		}
	})

	asRelease(t)
	var out bytes.Buffer
	if err := renderDryRun(context.Background(), &out, spec, userDataTestFinder(), "us-east-1"); err != nil {
		t.Fatalf("the dry run refused a staged 20 KB task: %v", err)
	}
	if !strings.Contains(out.String(), "are staged in S3, in this account's spawn-staging bucket") {
		t.Errorf("the dry run does not say the wrapper and flush hook are staged\n%s", out.String())
	}

	store := &stagingAWS{}
	prov := &recordingProvisioner{store: store}
	if _, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: store, Provisioner: prov}, spec, "us-east-1"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if prov.n != 1 || prov.opts.StagedFiles == nil {
		t.Fatalf("provisioned %d times with StagedFiles %+v; want one staged launch", prov.n, prov.opts.StagedFiles)
	}
	st := prov.opts.StagedFiles
	size, script := userDataOf(t, prov.cfg, prov.opts)
	if strings.Contains(script, "EOFSPAWNCMD") || strings.Contains(script, spec.Command[2][:200]) {
		t.Error("the user data still carries the command inline")
	}
	if !strings.Contains(script, st.SHA256) || !strings.Contains(script, st.URI) {
		t.Error("the user data does not carry the staged object's URI and sha256")
	}

	// The object uploaded is byte-identical to the one the user data verifies,
	// under the task's own results prefix, and was put before RunInstances.
	wantKey := taskiam.StagedFilesKey(spec.TaskID, st.SHA256)
	if st.URI != "s3://"+testStagingBucket+"/"+wantKey {
		t.Errorf("staged URI = %s, want s3://%s/%s", st.URI, testStagingBucket, wantKey)
	}
	if !strings.HasPrefix(wantKey, taskiam.StagedFilesRoot+spec.TaskID+"/bootstrap-") {
		t.Errorf("staged key %s is not under the task_id", wantKey)
	}
	if len(store.puts) != 1 {
		t.Errorf("uploaded %d objects, want exactly one, to the staging bucket: %v", len(store.puts), store.calls)
	}
	if indexOf(store.calls, "ensure "+testStagingBucket) < 0 || indexOf(store.calls, "ensure "+testStagingBucket) > indexOf(store.calls, "put "+wantKey) {
		t.Errorf("the staging bucket was not made sure of before the upload: %v", store.calls)
	}
	for _, o := range store.owners {
		if o != "123456789012" {
			t.Errorf("a staging-bucket call did not require the launching account as its owner: %q", store.owners)
			break
		}
	}
	body, ok := store.puts[testStagingBucket+"/"+wantKey]
	if !ok {
		t.Fatalf("nothing uploaded at %s; puts: %v", wantKey, store.calls)
	}
	if !bytes.Equal(body, launcher.StagedFilesObject(prov.cfg.JobArrayCommand, prov.opts.TaskFlushScript)) {
		t.Error("the uploaded object is not the bytes the bootstrap writes the files from")
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != st.SHA256 {
		t.Error("the uploaded object does not hash to the sha256 the user data checks")
	}
	if !strings.Contains(string(body), spec.Command[2][:200]) {
		t.Error("the uploaded object does not carry the command")
	}
	if !containsString(prov.callsAtRun, "put "+wantKey) {
		t.Errorf("the object was not uploaded before RunInstances; calls before it: %v", prov.callsAtRun)
	}
	t.Logf("20 KB command: user data %d of %d bytes, staged object %d bytes", size, aws.MaxUserDataBytes, len(body))
}

// TestStagedUserDataSizes reports the before/after user data of a trivial
// task and of one with a 20 KB command, and checks that staging never makes a
// task's user data bigger.
func TestStagedUserDataSizes(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{{"trivial task", 0}, {"20 KB command", largeCommandBytes}} {
		spec := userDataSpec(tc.n)
		measure := func(stage bool) (int, int, error) {
			n, obj, err := taskUserDataPreflight(spec, userDataTestSized, "us-east-1", stage)
			return n, obj, err
		}
		asDev(t)
		devBytes, _, devErr := measure(false)
		asRelease(t)
		inlineBytes, _, inlineErr := measure(false)
		stagedBytes, objBytes, err := measure(true)
		if err != nil {
			t.Fatalf("%s: staged: %v", tc.name, err)
		}
		t.Logf("%s: dev build (inline body+files) %d (%v); release, files inline %d (%v); release, files staged %d (object %d)",
			tc.name, devBytes, devErr, inlineBytes, inlineErr, stagedBytes, objBytes)
		if inlineErr == nil && stagedBytes >= inlineBytes {
			t.Errorf("%s: staging made the user data %d bytes, not smaller than %d inline", tc.name, stagedBytes, inlineBytes)
		}
	}
}

// TestStagingClearsAPreviousAttemptsObject: a re-run of a task_id deletes the
// staged objects earlier attempts left under its prefix, alongside the stale
// completion records and before this attempt uploads its own.
func TestStagingClearsAPreviousAttemptsObject(t *testing.T) {
	asRelease(t)
	spec := userDataSpec(100)
	old := taskiam.StagedFilesKey(spec.TaskID, strings.Repeat("a", 64))
	store := &stagingAWS{existed: []string{old}}
	prov := &recordingProvisioner{store: store}
	res, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: store, Provisioner: prov}, spec, "us-east-1")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(store.listed) != 1 || store.listed[0] != taskiam.StagedFilesKeyPrefix(spec.TaskID) || !containsString(store.calls, "list "+testStagingBucket+"/"+taskiam.StagedFilesKeyPrefix(spec.TaskID)) {
		t.Errorf("listed %v, want the task's own bootstrap- prefix in the staging bucket", store.calls)
	}
	_ = res
	newKey := taskiam.StagedFilesKey(spec.TaskID, prov.opts.StagedFiles.SHA256)
	del, put := indexOf(store.calls, "delete "+old), indexOf(store.calls, "put "+newKey)
	if del < 0 || put < 0 || del > put {
		t.Errorf("the previous attempt's object was not deleted before this one was staged: %v", store.calls)
	}
	if containsString(store.calls, "delete "+newKey) {
		t.Error("deleted this attempt's own object")
	}
}

// TestStagingFallsBackInlineWhenTheUploadFails: a launching principal that
// cannot write the results bucket (never needed before #1274) still launches
// a task that fits inline, with a warning, and gets an error only for one that
// cannot fit without staging.
func TestStagingFallsBackInlineWhenTheUploadFails(t *testing.T) {
	asRelease(t)
	denied := errors.New("AccessDenied")

	store := &stagingAWS{putErr: denied}
	prov := &recordingProvisioner{}
	var warn bytes.Buffer
	if _, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: store, Provisioner: prov, Warn: &warn}, userDataSpec(100), "us-east-1"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if prov.opts.StagedFiles != nil {
		t.Error("launched with a reference to an object that was never uploaded")
	}
	if _, script := userDataOf(t, prov.cfg, prov.opts); !strings.Contains(script, "EOFSPAWNCMD") {
		t.Error("the fallback did not put the command inline")
	}
	if !strings.Contains(warn.String(), "sending them in the user data instead") {
		t.Errorf("no warning about the fallback: %q", warn.String())
	}

	prov = &recordingProvisioner{}
	_, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: &stagingAWS{putErr: denied}, Provisioner: prov}, userDataSpec(largeCommandBytes), "us-east-1")
	if err == nil || !errors.Is(err, denied) || prov.n != 0 {
		t.Fatalf("Launch = %v after %d provisions; want the upload error and no launch", err, prov.n)
	}
}

// TestDevBuildAndPlainAWSStayInline: a dev build, and an embedder whose AWS
// has no S3 upload, launch exactly as before #1274: nothing listed, deleted
// beyond the completion records, or uploaded, and the files inline.
func TestDevBuildAndPlainAWSStayInline(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) AWS{
		"dev build":               func(t *testing.T) AWS { asDev(t); return &stagingAWS{} },
		"AWS without S3 upload":   func(t *testing.T) AWS { asRelease(t); return &fakeAWS{} },
		"dev build, no S3 upload": func(t *testing.T) AWS { asDev(t); return &fakeAWS{} },
	} {
		t.Run(name, func(t *testing.T) {
			client := setup(t)
			spec := userDataSpec(100)
			p, err := Plan(context.Background(), Deps{Finder: userDataTestFinder(), AWS: client}, spec, "us-east-1")
			if err != nil {
				t.Fatal(err)
			}
			if p.StagedBytes != 0 {
				t.Errorf("the plan reports %d staged bytes", p.StagedBytes)
			}
			prov := &recordingProvisioner{}
			if _, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: client, Provisioner: prov}, spec, "us-east-1"); err != nil {
				t.Fatal(err)
			}
			if prov.opts.StagedFiles != nil {
				t.Error("staged")
			}
			if s, ok := client.(*stagingAWS); ok && len(s.calls) != 2 {
				t.Errorf("object calls %v, want only the two completion-record deletes", s.calls)
			}
			_, script := userDataOf(t, prov.cfg, prov.opts)
			if !strings.Contains(script, "EOFSPAWNCMD") || !strings.Contains(script, "EOFSPAWNTASKFLUSH") {
				t.Error("the command and flush hook are not inline")
			}
		})
	}
}

// TestTaskRoleCanReadExactlyItsStagedObjects is the IAM gate: the policy a
// task's instance profile gets grants s3:GetObject in the staging bucket on
// the staged object's key shape and nothing else there, no s3:ListBucket on
// it at all, and no access to staged-looking keys in any other bucket. Checked
// by evaluating the policy's ARN patterns the way IAM matches them.
func TestTaskRoleCanReadExactlyItsStagedObjects(t *testing.T) {
	asRelease(t)
	for name, prefix := range map[string]string{
		"default prefix":        "",
		"custom results_prefix": "s3://lab-results/project-x/run-7",
		"bucket root":           "s3://lab-results",
	} {
		t.Run(name, func(t *testing.T) {
			spec := userDataSpec(100)
			spec.ResultsPrefix = prefix
			spec.Inputs = []taskproto.Manifest{{Source: "s3://inputs-bucket/data/", Destination: "/data/"}}
			spec.Resources.S3ReadWrite = []string{"s3://scratch-bucket"}
			store := &stagingAWS{}
			prov := &recordingProvisioner{}
			res, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: store, Provisioner: prov}, spec, "us-east-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(store.profiles) != 1 {
				t.Fatalf("profiles = %d", len(store.profiles))
			}
			pol := store.profiles[0].InlinePolicyJSON
			can := func(action, arn string) bool {
				for _, r := range policyResources(t, pol, action) {
					if iamGlob(r, arn) {
						return true
					}
				}
				return false
			}
			key := taskiam.StagedFilesKey(spec.TaskID, prov.opts.StagedFiles.SHA256)
			if !can("s3:GetObject", "arn:aws:s3:::"+testStagingBucket+"/"+key) {
				t.Errorf("the role cannot read its own staged object %s; policy %s", key, pol)
			}
			for _, a := range []string{"s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation"} {
				if can(a, "arn:aws:s3:::"+testStagingBucket) {
					t.Errorf("the role has %s on the staging bucket", a)
				}
			}
			sha := strings.Repeat("a", 64)
			for _, arn := range []string{
				"arn:aws:s3:::" + testStagingBucket + "/other-task/notes.txt",
				"arn:aws:s3:::" + testStagingBucket + "/bootstrap-short.sh",
				"arn:aws:s3:::" + testStagingBucket + "/t/bootstrap-" + sha + ".sh.bak",
				"arn:aws:s3:::" + res.ResultsBucket + "/" + path.Join(res.ResultsKeyPrefix, "bootstrap-"+sha+".sh"),
				"arn:aws:s3:::spawn-staging-999999999999-us-east-1/t/bootstrap-" + sha + ".sh",
			} {
				for _, a := range []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"} {
					if can(a, arn) && !strings.Contains(arn, res.ResultsBucket) {
						t.Errorf("the role has %s on %s", a, arn)
					}
				}
				if can("s3:GetObject", arn) && strings.Contains(arn, res.ResultsBucket) {
					t.Errorf("the role can read a staged-looking key in its results bucket: %s", arn)
				}
			}
		})
	}
}

// policyResources returns every Resource of every Allow statement in policy
// whose actions cover action (exactly, or by a trailing-* wildcard).
func policyResources(t *testing.T, policy, action string) []string {
	t.Helper()
	var doc struct {
		Statement []struct {
			Effect   string
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("policy is not JSON: %v\n%s", err, policy)
	}
	var out []string
	for _, s := range doc.Statement {
		if s.Effect != "Allow" {
			continue
		}
		for _, a := range s.Action {
			if iamGlob(a, action) {
				out = append(out, s.Resource...)
				break
			}
		}
	}
	return out
}

// iamGlob matches an IAM ARN pattern: * is any run of characters, slashes
// included, and ? is any one character.
func iamGlob(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if iamGlob(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && iamGlob(pattern[1:], s[1:])
	}
	return s != "" && s[0] == pattern[0] && iamGlob(pattern[1:], s[1:])
}

func containsString(list []string, s string) bool { return indexOf(list, s) >= 0 }

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// TestStagedGrantKeepsTheRoleShared: two tasks with the same buckets get the
// same instance policy, so the same role. A grant naming each task's own
// prefix would give every task_id its own role (CreateOrGetInstanceProfile
// names roles by the policy's hash), thousands for one workflow fan-out.
func TestStagedGrantKeepsTheRoleShared(t *testing.T) {
	asRelease(t)
	store := &stagingAWS{}
	for _, id := range []string{"task-a", "task-b"} {
		spec := userDataSpec(100)
		spec.TaskID = id
		if _, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: store, Provisioner: &recordingProvisioner{}}, spec, "us-east-1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.profiles) != 2 {
		t.Fatalf("profiles = %d", len(store.profiles))
	}
	a, b := store.profiles[0], store.profiles[1]
	if aws.PredictedInstanceRoleName(a) != aws.PredictedInstanceRoleName(b) {
		t.Errorf("two tasks with the same buckets get different roles:\n%s\n%s", a.InlinePolicyJSON, b.InlinePolicyJSON)
	}
}

// TestStagedKeyIsUnguessablePerRun: the shared GetObject grant is safe only
// because no task can name another's object, so every attempt's key must be
// new, even for the identical spec: the hashed bytes carry the attempt's
// random run id.
func TestStagedKeyIsUnguessablePerRun(t *testing.T) {
	asRelease(t)
	store := &stagingAWS{}
	var keys []string
	for i := 0; i < 2; i++ {
		prov := &recordingProvisioner{}
		res, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: store, Provisioner: prov}, userDataSpec(100), "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		key := taskiam.StagedFilesKey(res.TaskID, prov.opts.StagedFiles.SHA256)
		if !bytes.Contains(store.puts[testStagingBucket+"/"+key], []byte(res.RunID)) {
			t.Errorf("the staged object does not carry the attempt's run id %s", res.RunID)
		}
		keys = append(keys, key)
	}
	if keys[0] == keys[1] {
		t.Errorf("two launches of the same spec staged the same key %s; the key must be unguessable per run", keys[0])
	}
}

// TestPlanRefusesASpecNamingTheStagingBucket: Plan, and so Launch and the
// MCP server's dry run, refuse a spec that names a staging bucket even when
// its caller did not run TaskSpec.Validate, before any AWS call.
func TestPlanRefusesASpecNamingTheStagingBucket(t *testing.T) {
	spec := userDataSpec(100)
	spec.Inputs = []taskproto.Manifest{{Source: "s3://" + testStagingBucket + "/other-task/", Destination: "/x/"}}
	store := &stagingAWS{}
	prov := &recordingProvisioner{}
	_, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: store, Provisioner: prov}, spec, "us-east-1")
	if err == nil || !strings.Contains(err.Error(), "spawn staging bucket") {
		t.Fatalf("Launch = %v, want a refusal", err)
	}
	if prov.n != 0 || len(store.calls) != 0 || len(store.profiles) != 0 {
		t.Errorf("acted before refusing: calls %v, profiles %d, launches %d", store.calls, len(store.profiles), prov.n)
	}
}

// TestStagingFallsBackInlineWhenTheBucketCannotBeReady: a staging bucket that
// cannot be created or configured (or belongs to another account) is never
// written to. A task that fits inline launches inline with a warning; one that
// does not is refused, saying why.
func TestStagingFallsBackInlineWhenTheBucketCannotBeReady(t *testing.T) {
	asRelease(t)
	notOurs := errors.New("staging bucket is not usable by account 123456789012")

	store := &stagingAWS{ensureErr: notOurs}
	prov := &recordingProvisioner{}
	var warn bytes.Buffer
	if _, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: store, Provisioner: prov, Warn: &warn}, userDataSpec(100), "us-east-1"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if prov.opts.StagedFiles != nil || len(store.puts) != 0 || indexOf(store.calls, "list "+testStagingBucket+"/userdata-limit/bootstrap-") >= 0 {
		t.Errorf("used a staging bucket that could not be made ready: %v", store.calls)
	}
	if !strings.Contains(warn.String(), "sending them in the user data instead") {
		t.Errorf("no warning: %q", warn.String())
	}

	prov = &recordingProvisioner{}
	_, err := Launch(context.Background(), Deps{Finder: userDataTestFinder(), AWS: &stagingAWS{ensureErr: notOurs}, Provisioner: prov}, userDataSpec(largeCommandBytes), "us-east-1")
	if err == nil || !errors.Is(err, notOurs) || prov.n != 0 {
		t.Fatalf("Launch = %v after %d launches; want the staging-bucket error and no launch", err, prov.n)
	}
}
