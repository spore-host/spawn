package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// GitHub's refusal, as gh prints it when "Allow GitHub Actions to create and
// approve pull requests" is off: the one case that falls back to an issue.
const ghRefusal = "pull request create failed: GraphQL: GitHub Actions is not permitted to create or approve pull requests (createPullRequest)"

func TestPRRefusedOnlyForGitHubsRefusal(t *testing.T) {
	if !prRefused(ghRefusal) {
		t.Errorf("prRefused(%q) = false: the fallback would never run", ghRefusal)
	}
	if !prRefused("\n" + strings.ToUpper(ghRefusal) + "\n") {
		t.Error("prRefused is sensitive to case or surrounding lines")
	}
	// Every other failure must fail the run, not open an issue.
	for _, out := range []string{
		"",
		"HTTP 401: Bad credentials (https://api.github.com/graphql)",
		"pull request create failed: GraphQL: Resource not accessible by integration (createPullRequest)",
		"pull request create failed: GraphQL: No commits between main and bot/awscli-2.40.0 (createPullRequest)",
		"a pull request for branch \"bot/awscli-2.40.0\" into branch \"main\" already exists",
		"error connecting to api.github.com",
		"GitHub Actions is not permitted to push to this branch",
	} {
		if prRefused(out) {
			t.Errorf("prRefused(%q) = true: a real failure would open an issue and pass the run", out)
		}
	}
}

// The subcommand the workflow pipes gh's output into: success only for the
// refusal, errNotRefused (exit 1, silent) otherwise.
func TestPRRefusedSubcommand(t *testing.T) {
	if err := run([]string{"pr-refused"}, strings.NewReader(ghRefusal), &bytes.Buffer{}); err != nil {
		t.Errorf("pr-refused on GitHub's refusal: %v, want nil", err)
	}
	err := run([]string{"pr-refused"}, strings.NewReader("HTTP 401: Bad credentials"), &bytes.Buffer{})
	if !errors.Is(err, errNotRefused) {
		t.Errorf("pr-refused on a 401: %v, want errNotRefused", err)
	}
}

// The issue holds what the owner needs to open the PR: the compare link, the
// verified version, both sha256s and the signing fingerprint.
func TestFallbackIssueHoldsTheVerifiedPin(t *testing.T) {
	x, a := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	compare := "https://github.com/spore-host/spawn/compare/main...bot/awscli-2.40.0?expand=1"
	var title, body bytes.Buffer
	if err := run([]string{"issue-title", "--from", "2.39.3", "--version", "2.40.0"}, nil, &title); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(title.String()); got != "AWS CLI pin bump available: 2.39.3 → 2.40.0" {
		t.Errorf("title = %q", got)
	}
	args := []string{"issue-body", "--from", "2.39.3", "--version", "2.40.0", "--sha256-x86_64", x, "--sha256-aarch64", a, "--compare", compare}
	if err := run(args, nil, &body); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{compare, "`2.40.0`", x, a, "FB5DB77FD5C118B80511ADA8A6310ACC4672475C", "Open the PR from this link"} {
		if !strings.Contains(body.String(), want) {
			t.Errorf("issue body lacks %q:\n%s", want, body.String())
		}
	}
	// A body with a missing or swapped hash would send the owner to a PR whose
	// pin this issue misstates; refuse it.
	for _, bad := range [][]string{
		{"issue-body", "--from", "2.39.3", "--version", "2.40.0", "--sha256-x86_64", "", "--sha256-aarch64", a, "--compare", compare},
		{"issue-body", "--from", "2.39.3", "--version", "2.40.0", "--sha256-x86_64", x, "--sha256-aarch64", x, "--compare", compare},
		{"issue-body", "--from", "2.39.3", "--version", "2.40.0", "--sha256-x86_64", x, "--sha256-aarch64", a, "--compare", ""},
	} {
		if err := run(bad, nil, &bytes.Buffer{}); err == nil {
			t.Errorf("issue-body accepted %v", bad)
		}
	}
}
