package taskproto

import (
	"strings"
	"testing"
)

// fullSpec exercises every wrapper section: env, inputs (file + prefix), a
// command with shell-metachar args, and outputs.
func fullSpec() *TaskSpec {
	return &TaskSpec{
		TaskID:  "align-42",
		Command: []string{"bash", "-c", "echo $HOME; rm -rf /tmp/x && true"},
		Env:     map[string]string{"THREADS": "16", "REF": "/data/ref.fa"},
		Inputs: []Manifest{
			{Source: "s3://in-bucket/ref.fa", Destination: "/data/ref.fa"},
			{Source: "s3://in-bucket/reads/", Destination: "/work/reads/"},
		},
		Outputs: []Manifest{
			{Source: "/work/out.bam", Destination: "s3://out-bucket/out.bam"},
		},
		Lifecycle: Lifecycle{TTL: "4h", OnComplete: "terminate"},
	}
}

func TestGenerateWrapper_Structure(t *testing.T) {
	w := GenerateWrapper(fullSpec(), "spawn-results-123-us-east-1", "us-east-1")

	mustContain := []string{
		"#!/bin/bash",
		"set -u -o pipefail", // safe flags present
		"RESULTS_PREFIX='s3://spawn-results-123-us-east-1/tasks/align-42'",
		"rc=$?", // exit-code capture
		"aws s3 cp /tmp/spawn-completion.json \"$RESULTS_PREFIX/completion.json\"",
		"aws s3 cp /tmp/spawn.exitcode \"$RESULTS_PREFIX/.exitcode\"",
		"> /tmp/SPAWN_COMPLETE", // spored signal
		"exit $rc",
		"--recursive",         // prefix input gets --recursive
		"export THREADS='16'", // env exported + quoted
	}
	for _, sub := range mustContain {
		if !strings.Contains(w, sub) {
			t.Errorf("wrapper missing %q\n---\n%s", sub, w)
		}
	}

	// set -e must NOT be present (it would abort before stage-out/record).
	for _, line := range strings.Split(w, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -e" || strings.HasPrefix(trimmed, "set -e ") || trimmed == "set -eu" {
			t.Errorf("wrapper must not use 'set -e' (aborts before completion record): %q", line)
		}
	}
}

func TestGenerateWrapper_QuotesMetacharArgs(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "t",
		Command:   []string{"echo", "a; rm -rf /", "$(whoami)", "it's"},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	// Each arg must appear single-quoted; the single-quote in "it's" is escaped.
	for _, want := range []string{`'echo'`, `'a; rm -rf /'`, `'$(whoami)'`, `'it'\''s'`} {
		if !strings.Contains(w, want) {
			t.Errorf("expected quoted arg %q in wrapper\n---\n%s", want, w)
		}
	}
	// The dangerous forms must NOT appear unquoted as their own tokens.
	if strings.Contains(w, "( echo a; rm -rf / ") {
		t.Error("metachar arg leaked unquoted into the command subshell")
	}
}

func TestGenerateWrapper_StageOutAfterCommand(t *testing.T) {
	w := GenerateWrapper(fullSpec(), "b", "us-east-1")
	runIdx := strings.Index(w, "rc=$?")
	outIdx := strings.Index(w, "s3://out-bucket/out.bam")
	recIdx := strings.Index(w, "completion.json")
	if runIdx < 0 || outIdx < 0 || recIdx < 0 {
		t.Fatalf("wrapper missing expected sections")
	}
	if !(runIdx < outIdx && outIdx < recIdx) {
		t.Errorf("ordering wrong: want command(%d) < stage-out(%d) < record(%d)", runIdx, outIdx, recIdx)
	}
}

func TestGenerateWrapper_NoInputsNoOutputs(t *testing.T) {
	spec := &TaskSpec{TaskID: "t", Command: []string{"true"}, Lifecycle: Lifecycle{TTL: "1h"}}
	w := GenerateWrapper(spec, "b", "us-east-1")
	// Still writes the completion record + signals spored even with no staging.
	for _, sub := range []string{"completion.json", "/tmp/SPAWN_COMPLETE", "exit $rc"} {
		if !strings.Contains(w, sub) {
			t.Errorf("minimal wrapper missing %q", sub)
		}
	}
}

func TestGenerateWrapper_HostHasNoDocker(t *testing.T) {
	// A container-less task must never emit docker/ECR machinery.
	w := GenerateWrapper(fullSpec(), "b", "us-east-1")
	for _, bad := range []string{"docker run", "docker pull", "dnf install -y docker", "ecr get-login-password"} {
		if strings.Contains(w, bad) {
			t.Errorf("host task wrapper unexpectedly contains %q", bad)
		}
	}
	if !strings.Contains(w, "( 'bash'") {
		t.Errorf("host task should run argv in a subshell")
	}
}

func TestGenerateWrapper_PublicContainer(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "align",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bwa", "mem", "/data/ref.fa"},
		Inputs:    []Manifest{{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"}},
		Outputs:   []Manifest{{Source: "/work/out.bam", Destination: "s3://out/out.bam"}},
		Lifecycle: Lifecycle{TTL: "4h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	mustContain := []string{
		"command -v docker",                  // install guard
		"sudo dnf install -y docker",         // install (root)
		"sudo systemctl enable --now docker", // start daemon (root)
		"sudo docker info",                   // bounded wait for the socket
		"sudo docker pull 'quay.io/biocontainers/bwa:0.7.18'",
		"sudo docker run --rm ",                                         // run
		"'quay.io/biocontainers/bwa:0.7.18' 'bwa' 'mem' '/data/ref.fa'", // image + argv
		"-v '/data':'/data'",                                            // input dir mount
		"-v '/work':'/work'",                                            // output dir mount
		"aws s3 cp 's3://in/ref.fa' '/data/ref.fa'",                     // stage-in still on host
		"completion.json",                                               // record unchanged
	}
	for _, sub := range mustContain {
		if !strings.Contains(w, sub) {
			t.Errorf("public-container wrapper missing %q\n---\n%s", sub, w)
		}
	}
	// Public image: NO ECR login, NO --gpus.
	if strings.Contains(w, "ecr get-login-password") {
		t.Error("public image should not emit an ECR login")
	}
	if strings.Contains(w, "--gpus") {
		t.Error("no GPU requested; --gpus must not appear")
	}
}

func TestGenerateWrapper_PrivateECRContainerWithGPU(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "infer",
		Container: "123456789012.dkr.ecr.us-west-2.amazonaws.com/model:v3",
		Command:   []string{"python", "infer.py"},
		Resources: ResourceRequest{GPUs: 1},
		Lifecycle: Lifecycle{TTL: "2h"},
	}
	w := GenerateWrapper(spec, "b", "us-west-2")

	if !strings.Contains(w, "aws ecr get-login-password --region 'us-west-2' | sudo docker login --username AWS --password-stdin '123456789012.dkr.ecr.us-west-2.amazonaws.com'") {
		t.Errorf("private ECR image must emit a docker login to its registry host\n---\n%s", w)
	}
	if !strings.Contains(w, "sudo docker run --rm --gpus all ") {
		t.Errorf("GPU task must pass --gpus all\n---\n%s", w)
	}
}

func TestContainerMountDirs(t *testing.T) {
	spec := &TaskSpec{
		Inputs: []Manifest{
			{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"},
			{Source: "s3://in/x", Destination: "/data/sub/x"}, // /data/sub distinct from /data
		},
		Outputs: []Manifest{
			{Source: "/work/out.bam", Destination: "s3://out/out.bam"},
			{Source: "/data/also.txt", Destination: "s3://out/also.txt"}, // /data again → deduped
		},
	}
	got := containerMountDirs(spec)
	want := []string{"/data", "/data/sub", "/work"} // sorted, deduped
	if len(got) != len(want) {
		t.Fatalf("containerMountDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("containerMountDirs = %v, want %v", got, want)
			break
		}
	}
}

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"simple": "'simple'",
		"a b":    "'a b'",
		"it's":   `'it'\''s'`,
		"$(x)":   "'$(x)'",
		"`cmd`":  "'`cmd`'",
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
