package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func TestTaskLaunchConfig(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "align-42",
		Command:   []string{"bwa", "mem"},
		Resources: taskproto.ResourceRequest{Purchase: taskproto.PurchaseSpot},
		Lifecycle: taskproto.Lifecycle{TTL: "4h"}, // OnComplete empty → defaults to terminate
	}
	sized := &taskproto.SizeResult{InstanceType: "c7i.4xlarge"}
	cfg := taskLaunchConfig(spec, sized, "us-east-1", "spawn-task-profile", "#!/bin/bash\ntrue\n")

	if cfg.InstanceType != "c7i.4xlarge" {
		t.Errorf("InstanceType = %q", cfg.InstanceType)
	}
	if !cfg.Spot {
		t.Error("Spot should be true for purchase=spot")
	}
	if cfg.TTL != "4h" {
		t.Errorf("TTL = %q", cfg.TTL)
	}
	if cfg.OnComplete != "terminate" {
		t.Errorf("OnComplete = %q, want terminate (default)", cfg.OnComplete)
	}
	if cfg.Name != "align-42" || cfg.Tags["spawn:task-id"] != "align-42" {
		t.Errorf("task-id not stamped: Name=%q tag=%q", cfg.Name, cfg.Tags["spawn:task-id"])
	}
	if cfg.IamInstanceProfile != "spawn-task-profile" {
		t.Errorf("IamInstanceProfile = %q", cfg.IamInstanceProfile)
	}
	if !strings.Contains(cfg.JobArrayCommand, "#!/bin/bash") {
		t.Errorf("JobArrayCommand should carry the wrapper, got %q", cfg.JobArrayCommand)
	}
	// AMI/UserData must be left empty for Provision to fill.
	if cfg.AMI != "" || cfg.UserData != "" {
		t.Errorf("AMI/UserData should be empty for Provision, got AMI=%q UserData=%q", cfg.AMI, cfg.UserData)
	}
	// Completion file must be tagged explicitly (spawn#406) so on-complete
	// teardown doesn't depend on provider-side defaulting / tag-visibility timing.
	if cfg.CompletionFile != "/tmp/SPAWN_COMPLETE" {
		t.Errorf("CompletionFile = %q, want /tmp/SPAWN_COMPLETE", cfg.CompletionFile)
	}
	if cfg.CompletionDelay == "" {
		t.Error("CompletionDelay should be set so on-complete has a bounded grace")
	}
}

func TestTaskLaunchConfig_OnDemandDefault(t *testing.T) {
	spec := &taskproto.TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: taskproto.Lifecycle{TTL: "1h"}}
	cfg := taskLaunchConfig(spec, &taskproto.SizeResult{InstanceType: "m7i.large"}, "us-east-1", "p", "w")
	if cfg.Spot {
		t.Error("Spot should default to false when purchase is unset")
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
