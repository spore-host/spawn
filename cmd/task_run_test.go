package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

type fakeTaskFinder struct{ cands []taskproto.Candidate }

func (f fakeTaskFinder) FindCandidates(context.Context, taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	return f.cands, nil
}

func TestTaskExtraPolicies(t *testing.T) {
	cases := []struct {
		name  string
		spec  *taskproto.TaskSpec
		wantN int
		want  string
	}{
		{"no container", &taskproto.TaskSpec{}, 0, ""},
		{"public container", &taskproto.TaskSpec{Container: "quay.io/biocontainers/bwa:0.7.18"}, 0, ""},
		{"private ECR", &taskproto.TaskSpec{Container: "123456789012.dkr.ecr.us-east-1.amazonaws.com/x:v1"}, 1, "ecr:ReadOnly"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := taskExtraPolicies(tc.spec)
			if len(got) != tc.wantN {
				t.Fatalf("taskExtraPolicies = %v, want %d entries", got, tc.wantN)
			}
			if tc.wantN > 0 && got[0] != tc.want {
				t.Errorf("taskExtraPolicies = %v, want [%s]", got, tc.want)
			}
		})
	}
}

func TestRenderTaskDryRun(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "align-42",
		Command:   []string{"bwa", "mem", "ref.fa"},
		Resources: taskproto.ResourceRequest{CPU: 16, MemoryGiB: 32},
		Inputs:    []taskproto.Manifest{{Source: "s3://b/in.fq", Destination: "/work/in.fq"}},
		Lifecycle: taskproto.Lifecycle{TTL: "4h", OnComplete: "terminate"},
	}
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "c7a.4xlarge", Family: "c7a", VCPUs: 16, MemoryGiB: 32, OnDemandPrice: 0.62},
		{InstanceType: "c7i.4xlarge", Family: "c7i", VCPUs: 16, MemoryGiB: 32, OnDemandPrice: 0.71},
	}}

	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, spec, finder, "us-east-1"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()

	wants := []string{
		"DRY RUN — nothing will be launched.",
		"Task:         align-42",
		"bwa mem ref.fa",
		"c7a.4xlarge",          // cheapest of the two
		"Max cost:     ~$2.48", // 0.62 × 4h
		"s3://b/in.fq → /work/in.fq",
		"Re-run without --dry-run to launch this task.",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("dry-run output missing %q\n--- output ---\n%s", w, got)
		}
	}
	// Must never claim to have launched.
	if strings.Contains(got, "Instance i-") || strings.Contains(strings.ToLower(got), "launched instance") {
		t.Errorf("dry-run must not launch anything:\n%s", got)
	}
}

// TestRenderTaskDryRun_DiskAndCostLimit covers #556's dry-run visibility asks
// for items 2 and 3: an explicit resources.disk_gib and lifecycle.cost_limit
// must both show up in the preview, and the family-generation note (item 4)
// must appear for a known older-generation family.
func TestRenderTaskDryRun_DiskAndCostLimit(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "align-42",
		Command:   []string{"bwa", "mem", "ref.fa"},
		Resources: taskproto.ResourceRequest{CPU: 8, MemoryGiB: 16, DiskGiB: 100},
		Lifecycle: taskproto.Lifecycle{TTL: "4h", OnComplete: "terminate", CostLimit: 5},
	}
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "a1.2xlarge", Family: "a1", VCPUs: 8, MemoryGiB: 16, OnDemandPrice: 0.204},
	}}
	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, spec, finder, "us-east-1"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()
	wants := []string{
		"Root disk:    100 GiB (resources.disk_gib)",
		"Cost limit:   $5.00 (lifecycle.cost_limit)",
		"a1", "Graviton 1",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("dry-run output missing %q\n--- output ---\n%s", w, got)
		}
	}
}

// TestRenderTaskDryRun_DiskOmittedShowsAMIDefault covers the other half: when
// resources.disk_gib is NOT set, the preview must still say something (the AMI
// default), not stay silent — the issue's complaint was that nothing in the
// preview would tip an author off to the 8 GiB AL2023-arm64 default.
func TestRenderTaskDryRun_DiskOmittedShowsAMIDefault(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "align-42",
		Command:   []string{"bwa", "mem", "ref.fa"},
		Resources: taskproto.ResourceRequest{CPU: 8, MemoryGiB: 16},
		Lifecycle: taskproto.Lifecycle{TTL: "4h"},
	}
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "c7i.2xlarge", Family: "c7i", VCPUs: 8, MemoryGiB: 16, OnDemandPrice: 0.34},
	}}
	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, spec, finder, "us-east-1"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Root disk:    AMI default") {
		t.Errorf("dry-run output should show the AMI-default root disk when resources.disk_gib is unset\n--- output ---\n%s", got)
	}
	if strings.Contains(got, "Cost limit:") {
		t.Errorf("dry-run output should not show a cost limit line when lifecycle.cost_limit is unset\n--- output ---\n%s", got)
	}
}

func TestRenderTaskDryRun_Container(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "infer",
		Container: "123456789012.dkr.ecr.us-east-1.amazonaws.com/model:v3",
		Command:   []string{"python", "infer.py"},
		Resources: taskproto.ResourceRequest{CPU: 4, MemoryGiB: 16, GPUs: 1},
		Lifecycle: taskproto.Lifecycle{TTL: "2h"},
	}
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "g5.xlarge", Family: "g5", VCPUs: 4, MemoryGiB: 16, GPUs: 1, OnDemandPrice: 1.006},
	}}
	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, spec, finder, "us-east-1"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()
	for _, w := range []string{"model:v3", "container (docker run", "--gpus all", "private ECR"} {
		if !strings.Contains(got, w) {
			t.Errorf("container dry-run missing %q\n---\n%s", w, got)
		}
	}
}

// TestRenderTaskDryRun_GPUAMISelected covers spawn#601: a GPU-family instance must
// show the NVIDIA GPU DLAMI in the dry-run AMI line — NOT a driverless default —
// so an author can see `--gpus all` will land on a host with a driver. Regression
// guard for the issue's confusion (the AMI selection was invisible in the preview).
func TestRenderTaskDryRun_GPUAMISelected(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "gpu-ami-repro",
		Container: "nvcr.io/hpc/gromacs:latest",
		Command:   []string{"bash", "-c", "nvidia-smi"},
		Resources: taskproto.ResourceRequest{CPU: 8, MemoryGiB: 32, Families: []string{"g5"}},
		Lifecycle: taskproto.Lifecycle{TTL: "10m", OnComplete: "terminate"},
	}
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "g5.2xlarge", Family: "g5", VCPUs: 8, MemoryGiB: 32, GPUs: 1, OnDemandPrice: 1.212},
	}}
	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, spec, finder, "us-west-2"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()
	// The AMI line must name the GPU DLAMI, and must not present the GPU instance's
	// AMI as an unqualified default (the pre-#601 behavior the reporter inferred).
	if !strings.Contains(got, "AMI:          AL2023 GPU DLAMI") {
		t.Errorf("GPU dry-run must show the GPU DLAMI on the AMI line\n--- output ---\n%s", got)
	}
	if !strings.Contains(got, "NVIDIA driver") {
		t.Errorf("GPU dry-run AMI line should mention the NVIDIA driver\n--- output ---\n%s", got)
	}
	if strings.Contains(got, "AMI:          AL2023 default") {
		t.Errorf("GPU dry-run must not select the driverless default AL2023 AMI\n--- output ---\n%s", got)
	}
}

// TestRenderTaskDryRun_ExplicitAMIOverrides covers the override half of spawn#601:
// an explicit placement.ami (equivalently the --ami flag, which the RunE copies
// into it) always wins over auto-selection, even for a GPU family.
func TestRenderTaskDryRun_ExplicitAMIOverrides(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "pinned-ami",
		Command:   []string{"nvidia-smi"},
		Resources: taskproto.ResourceRequest{CPU: 8, MemoryGiB: 32, Families: []string{"g5"}},
		Placement: taskproto.Placement{AMI: "ami-0custompinned"},
		Lifecycle: taskproto.Lifecycle{TTL: "10m"},
	}
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "g5.2xlarge", Family: "g5", VCPUs: 8, MemoryGiB: 32, GPUs: 1, OnDemandPrice: 1.212},
	}}
	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, spec, finder, "us-west-2"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "AMI:          ami-0custompinned") {
		t.Errorf("explicit AMI must be shown verbatim on the AMI line\n--- output ---\n%s", got)
	}
	// Auto-selection (GPU DLAMI) must NOT appear — the explicit AMI overrides it.
	if strings.Contains(got, "AL2023 GPU DLAMI") {
		t.Errorf("explicit AMI must override GPU auto-selection\n--- output ---\n%s", got)
	}
}

// TestRenderTaskDryRun_NonGPUUnchanged covers the untouched half of spawn#601: a
// CPU instance still resolves the standard AL2023 for its architecture and never
// the GPU DLAMI.
func TestRenderTaskDryRun_NonGPUUnchanged(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "cpu-task",
		Command:   []string{"bwa", "mem"},
		Resources: taskproto.ResourceRequest{CPU: 16, MemoryGiB: 32},
		Lifecycle: taskproto.Lifecycle{TTL: "4h"},
	}
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "c7i.4xlarge", Family: "c7i", VCPUs: 16, MemoryGiB: 32, OnDemandPrice: 0.71},
	}}
	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, spec, finder, "us-east-1"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "AMI:          AL2023 default — x86_64 (auto-selected)") {
		t.Errorf("CPU dry-run should show the standard AL2023 AMI for x86_64\n--- output ---\n%s", got)
	}
	if strings.Contains(got, "GPU DLAMI") {
		t.Errorf("CPU dry-run must not select a GPU AMI\n--- output ---\n%s", got)
	}
}

func TestWaitDeadline(t *testing.T) {
	// A valid TTL yields now+TTL+slack (within a small tolerance).
	got := waitDeadline("1h")
	want := time.Now().Add(1*time.Hour + 2*time.Minute)
	if diff := got.Sub(want); diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("waitDeadline(1h) off by %s", diff)
	}
	// An unparseable/empty TTL falls back to a generous cap (>= ~24h out).
	if waitDeadline("nonsense").Before(time.Now().Add(23 * time.Hour)) {
		t.Error("waitDeadline fallback should be far in the future")
	}
	if waitDeadline("").Before(time.Now().Add(23 * time.Hour)) {
		t.Error("waitDeadline empty should use the fallback cap")
	}
}

func TestS3Bucket(t *testing.T) {
	cases := map[string]string{
		"s3://my-bucket/key":    "my-bucket",
		"s3://my-bucket":        "my-bucket",
		"s3://my-bucket/a/b/c":  "my-bucket",
		"/local/path":           "",
		"":                      "",
		"https://example.com/x": "",
	}
	for in, want := range cases {
		if got := s3Bucket(in); got != want {
			t.Errorf("s3Bucket(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestS3Buckets_DistinctS3Only(t *testing.T) {
	ms := []taskproto.Manifest{
		{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"},
		{Source: "s3://in/reads/", Destination: "/work/reads/"}, // same bucket → deduped
		{Source: "/work/out.bam", Destination: "s3://out/out.bam"},
	}
	got := s3Buckets(ms)
	// inputs: "in" (twice, deduped); the third's destination "out" is also s3.
	want := map[string]bool{"in": true, "out": true}
	if len(got) != 2 {
		t.Fatalf("s3Buckets = %v, want 2 distinct", got)
	}
	for _, b := range got {
		if !want[b] {
			t.Errorf("unexpected bucket %q in %v", b, got)
		}
	}
}

func TestTaskStagingPolicy(t *testing.T) {
	pol := taskStagingPolicy([]string{"in-bucket"}, []string{"out-bucket"}, "spawn-results-1-us-east-1", nil)

	// Read on the input bucket, write on output + results.
	wants := []string{
		`"arn:aws:s3:::in-bucket/*"`,
		`"arn:aws:s3:::in-bucket"`, // ListBucket target
		`"s3:GetObject"`,
		`"arn:aws:s3:::out-bucket/*"`,
		`"arn:aws:s3:::spawn-results-1-us-east-1/*"`,
		`"s3:PutObject"`,
	}
	for _, w := range wants {
		if !strings.Contains(pol, w) {
			t.Errorf("policy missing %q\n%s", w, pol)
		}
	}
	// Must be scoped — no wildcard resource.
	if strings.Contains(pol, `"Resource":"*"`) || strings.Contains(pol, `"Resource": "*"`) {
		t.Errorf("policy must not grant Resource *:\n%s", pol)
	}
	// The input bucket must NOT get write.
	if strings.Contains(pol, `"s3:PutObject"`) && strings.Contains(pol, `"arn:aws:s3:::in-bucket/*"`) {
		// only a problem if PutObject's resource list includes in-bucket; verify it doesn't
		// by checking the PutObject statement segment.
		put := pol[strings.Index(pol, `"s3:PutObject"`):]
		if strings.Contains(put, "in-bucket") {
			t.Errorf("input bucket must not receive write access:\n%s", pol)
		}
	}

	// The generated policy must always be valid JSON (it's string-built).
	if !json.Valid([]byte(pol)) {
		t.Fatalf("policy is not valid JSON:\n%s", pol)
	}
	// Also valid with no inputs (write-only statement).
	if !json.Valid([]byte(taskStagingPolicy(nil, nil, "res", nil))) {
		t.Fatalf("no-input policy is not valid JSON")
	}
}

func TestTaskStagingPolicyReadWriteBuckets(t *testing.T) {
	// A read-write bucket (e.g. Snakemake's S3 storage) gets full object access +
	// bucket-level ListBucket — the exact grant its plugin needs.
	pol := taskStagingPolicy(nil, nil, "spawn-results-1-us-east-1", []string{"storage-bucket"})
	wants := []string{
		`"arn:aws:s3:::storage-bucket/*"`, // object-level Get/Put/Delete
		`"arn:aws:s3:::storage-bucket"`,   // bucket-level ListBucket
		`"s3:DeleteObject"`,
		`"s3:ListBucket"`,
	}
	for _, w := range wants {
		if !strings.Contains(pol, w) {
			t.Errorf("read-write policy missing %q\n%s", w, pol)
		}
	}
	if !json.Valid([]byte(pol)) {
		t.Fatalf("read-write policy is not valid JSON:\n%s", pol)
	}
	if strings.Contains(pol, `"Resource":"*"`) {
		t.Errorf("policy must not grant Resource *:\n%s", pol)
	}
}

func TestS3Buckets2(t *testing.T) {
	got := s3Buckets2([]string{"s3://a/x", "s3://a/y", "s3://b", "not-s3"})
	if len(got) != 2 {
		t.Fatalf("s3Buckets2 = %v, want [a b]", got)
	}
}

// ── spawn#608: --wait must never report a previous attempt's completion ──────
//
// Re-running the same task_id overwrites the same
// tasks/<task_id>/completion.json key, so before this fix `--wait` could return
// the PREVIOUS attempt's verdict (with its old timestamps) while the new instance
// was still running — a false negative in the one authoritative place, and one
// that shows up exactly when someone re-runs after a fix.

// fakeResultStore is an offline taskResultStore: GetS3Object replays a scripted
// queue of responses (the last one repeats, so a poll loop can settle), and
// DeleteS3Object records the keys it was asked to clear. Zero AWS, zero launches.
type fakeResultStore struct {
	gets    []fakeGet
	getN    int
	deleted []string
	delErr  error
}

type fakeGet struct {
	body []byte
	err  error
}

func (f *fakeResultStore) GetS3Object(_ context.Context, _, _, _ string) ([]byte, error) {
	i := f.getN
	if i >= len(f.gets) {
		i = len(f.gets) - 1
	}
	f.getN++
	g := f.gets[i]
	return g.body, g.err
}

func (f *fakeResultStore) DeleteS3Object(_ context.Context, _, _, key string) error {
	f.deleted = append(f.deleted, key)
	return f.delErr
}

// missingKey is what pkg/aws returns for an object that isn't there — the "task
// still running" signal fetchCompletion translates to present=false.
func missingKey() error { return fmt.Errorf("%w: s3://b/k", aws.ErrS3NoSuchKey) }

func recordJSON(t *testing.T, rec taskproto.CompletionRecord) []byte {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return data
}

// TestPollCompletion_IgnoresPreviousRunRecord is the core regression guard: a
// record whose run_id belongs to an earlier attempt must NOT be returned — the
// poll keeps going until this run's record shows up.
func TestPollCompletion_IgnoresPreviousRunRecord(t *testing.T) {
	stale := recordJSON(t, taskproto.CompletionRecord{
		TaskID: "cookbook-plink-r1", RunID: "run-OLD", ExitCode: 141,
		State: taskproto.StateFailed, StartedAt: "2026-09-20T03:45:12Z", EndedAt: "2026-09-20T03:45:57Z",
	})
	fresh := recordJSON(t, taskproto.CompletionRecord{
		TaskID: "cookbook-plink-r1", RunID: "run-NEW", ExitCode: 0,
		State: taskproto.StateCompleted, StartedAt: "2026-09-20T03:49:06Z", EndedAt: "2026-09-20T03:50:00Z",
	})
	store := &fakeResultStore{gets: []fakeGet{
		{body: stale}, // the previous attempt's record is sitting at the key
		{body: stale}, // still there on the next poll
		{body: fresh}, // this run's record finally lands
	}}

	var warn bytes.Buffer
	rec, err := pollCompletion(context.Background(), store, "us-west-2", "spawn-results-1-us-west-2", "tasks", "cookbook-plink-r1", "run-NEW", time.Millisecond, time.Now().Add(time.Minute), &warn)
	if err != nil {
		t.Fatalf("pollCompletion: %v", err)
	}
	if rec.RunID != "run-NEW" {
		t.Fatalf("returned run_id = %q, want run-NEW (the stale record must never be reported)", rec.RunID)
	}
	if rec.ExitCode != 0 || rec.State != taskproto.StateCompleted {
		t.Errorf("returned the wrong record: %+v", rec)
	}
	// The skip must be announced once, not silently.
	if !strings.Contains(warn.String(), "previous run") {
		t.Errorf("expected a notice about ignoring a previous run's record, got: %q", warn.String())
	}
	if n := strings.Count(warn.String(), "previous run"); n != 1 {
		t.Errorf("stale notice printed %d times, want exactly 1 (not once per poll)", n)
	}
}

// TestPollCompletion_AcceptsMatchingRunID: the happy path — a record stamped with
// this run's id is returned immediately.
func TestPollCompletion_AcceptsMatchingRunID(t *testing.T) {
	body := recordJSON(t, taskproto.CompletionRecord{
		TaskID: "t", RunID: "run-A", ExitCode: 0, State: taskproto.StateCompleted,
	})
	store := &fakeResultStore{gets: []fakeGet{{err: missingKey()}, {body: body}}}
	var warn bytes.Buffer
	rec, err := pollCompletion(context.Background(), store, "us-east-1", "b", "tasks", "t", "run-A",
		time.Millisecond, time.Now().Add(time.Minute), &warn)
	if err != nil {
		t.Fatalf("pollCompletion: %v", err)
	}
	if rec.RunID != "run-A" {
		t.Errorf("run_id = %q, want run-A", rec.RunID)
	}
	if warn.Len() != 0 {
		t.Errorf("a matching record must not warn, got: %q", warn.String())
	}
}

// TestPollCompletion_EmptyRunIDAcceptedWithWarning locks in the documented
// back-compat choice: a record with NO run_id predates run-id stamping, so it
// cannot be attributed to this run. We accept it (hanging until TTL on every
// legitimate old-wrapper run would be worse) but say so loudly — the warning is
// the contract, so it is asserted here.
func TestPollCompletion_EmptyRunIDAcceptedWithWarning(t *testing.T) {
	body := recordJSON(t, taskproto.CompletionRecord{
		TaskID: "t", ExitCode: 3, State: taskproto.StateFailed, // no RunID
	})
	store := &fakeResultStore{gets: []fakeGet{{body: body}}}
	var warn bytes.Buffer
	rec, err := pollCompletion(context.Background(), store, "us-east-1", "b", "tasks", "t", "run-A",
		time.Millisecond, time.Now().Add(time.Minute), &warn)
	if err != nil {
		t.Fatalf("pollCompletion: %v", err)
	}
	if rec == nil || rec.ExitCode != 3 {
		t.Fatalf("an unattributable record should still be accepted, got %+v", rec)
	}
	for _, want := range []string{"no run_id", "cannot be attributed"} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("warning missing %q, got: %q", want, warn.String())
		}
	}
}

// TestPollCompletion_StaleOnlyTimesOutWithAClearMessage: if the ONLY record that
// ever appears belongs to an earlier attempt, --wait must time out rather than
// report it, and the error must name the situation so nobody re-debugs working
// code (the issue's actual cost).
func TestPollCompletion_StaleOnlyTimesOutWithAClearMessage(t *testing.T) {
	body := recordJSON(t, taskproto.CompletionRecord{
		TaskID: "t", RunID: "run-OLD", ExitCode: 141, State: taskproto.StateFailed,
	})
	store := &fakeResultStore{gets: []fakeGet{{body: body}}}
	var warn bytes.Buffer
	_, err := pollCompletion(context.Background(), store, "us-east-1", "b", "tasks", "t", "run-NEW",
		time.Millisecond, time.Now().Add(-time.Second), &warn)
	if err == nil {
		t.Fatal("expected a timeout rather than the previous attempt's record")
	}
	if !strings.Contains(err.Error(), "earlier attempt") {
		t.Errorf("timeout error should explain the stale record, got: %v", err)
	}
}

// TestPollCompletion_NoRunIDCallerKeepsOldBehavior: a caller with no run identity
// of its own (runID "") has nothing to compare against, so it must keep the
// pre-#608 behavior of accepting whatever is at the key — never hang.
func TestPollCompletion_NoRunIDCallerKeepsOldBehavior(t *testing.T) {
	body := recordJSON(t, taskproto.CompletionRecord{TaskID: "t", RunID: "run-whatever", ExitCode: 0})
	store := &fakeResultStore{gets: []fakeGet{{body: body}}}
	rec, err := pollCompletion(context.Background(), store, "us-east-1", "b", "tasks", "t", "",
		time.Millisecond, time.Now().Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("pollCompletion: %v", err)
	}
	if rec.RunID != "run-whatever" {
		t.Errorf("run_id = %q, want the record returned unfiltered", rec.RunID)
	}
}

func TestClassifyCompletionRun(t *testing.T) {
	cases := []struct {
		name  string
		rec   *taskproto.CompletionRecord
		runID string
		want  completionRunVerdict
	}{
		{"same run", &taskproto.CompletionRecord{RunID: "a"}, "a", completionForThisRun},
		{"different run", &taskproto.CompletionRecord{RunID: "b"}, "a", completionPreviousRun},
		{"no run_id (pre-#608 wrapper)", &taskproto.CompletionRecord{}, "a", completionUnattributed},
		{"caller has no run id", &taskproto.CompletionRecord{RunID: "b"}, "", completionForThisRun},
		{"no record", nil, "a", completionPreviousRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCompletionRun(tc.rec, tc.runID); got != tc.want {
				t.Errorf("classifyCompletionRun = %v, want %v", got, tc.want)
			}
		})
	}
}
