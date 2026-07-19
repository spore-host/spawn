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
	w := GenerateWrapper(fullSpec(), "spawn-results-123-us-east-1")

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
	w := GenerateWrapper(spec, "b")

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
	w := GenerateWrapper(fullSpec(), "b")
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
	w := GenerateWrapper(spec, "b")
	// Still writes the completion record + signals spored even with no staging.
	for _, sub := range []string{"completion.json", "/tmp/SPAWN_COMPLETE", "exit $rc"} {
		if !strings.Contains(w, sub) {
			t.Errorf("minimal wrapper missing %q", sub)
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
