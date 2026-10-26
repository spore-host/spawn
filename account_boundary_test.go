package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

// No ARN anywhere in shipped source, policy or template may name an AWS account
// outside the set this project owns.
//
// This encodes a constraint that was previously carried only by review, and
// review missed it: two committed scripts granted
// `sts:AssumeRole` on `arn:aws:iam::942542972736:role/SpawnDashboardCrossAccountRole`
// and the statement reached the live `SpawnDashboardLambdaPolicy`, where it sat
// attached to the execution role of four production functions. 942542972736 is
// an Amazon-internal account and an AWS Organizations management account; the
// project rule is that no spore.host-owned account may be granted trust into or
// access to it, and `sts:ExternalId` or tag-gating do not mitigate that.
//
// The shape is deliberately an ALLOWLIST of accounts rather than a denylist of
// forbidden ones. A denylist only catches the account someone already thought
// of; an allowlist catches a typo, a copied snippet from another org, and the
// next forbidden account nobody has heard of yet. That is the same reasoning as
// scripts/nested-modules.sh discovering modules instead of listing them.
func TestNoARNNamesAnAccountWeDoNotOwn(t *testing.T) {
	// Every account spore.host operates. Adding an entry here is a deliberate,
	// reviewable act — which is the point.
	allowed := map[string]string{
		"966362334030": "spore-host-infra — Lambdas, DynamoDB, S3, SNS",
		"435415984226": "spore-host-dev — EC2 launches",
		"812107987990": "dedicated spore.host infra account (#276)",
	}

	// An account id only matters in an ARN position. A bare 12-digit number is
	// often a doc example ("Example: 942542972736 -> c0zxr0ao" in
	// pkg/aws/tags.go) or a base36 encoding fixture, and flagging those would
	// make the gate noisy enough to be disabled — which is worse than not
	// having it.
	arnAccount := regexp.MustCompile(`arn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:(\d{12}):`)

	// Interpolation placeholders are not accounts.
	placeholder := regexp.MustCompile(`^[0#*$]+$`)

	// AWS's canonical documentation example account. It appears in help text
	// (cmd/image.go:108) and in a format comment
	// (lambda/dashboard-websocket-processor/main.go:138), which are legitimate.
	//
	// Permitted in Go source ONLY. A policy document, template or deploy script
	// has no business naming an example account, and allowing it everywhere
	// would reopen the hole this gate exists to close — a grant is a grant
	// whichever account it names.
	const docExampleAccount = "123456789012"

	type hit struct {
		file, account, line string
		n                   int
	}
	var bad []hit
	scanned := 0

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			if treewalk.Vanished(root, path, werr) {
				return nil
			}
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			// bin/ and docs-gen/ are build output; testdata/ and .git/ are not
			// shipped. docs/ and CHANGELOG.md are prose and are allowed to
			// quote a historical ARN.
			case ".git", "bin", "docs-gen", "testdata", "node_modules", "vendor", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(path, "_test.go"):
			// Test fixtures legitimately use arbitrary account ids as sample
			// data, including this one. Tests do not deploy anything.
			return nil
		case strings.HasSuffix(path, ".go"),
			strings.HasSuffix(path, ".sh"),
			strings.HasSuffix(path, ".yaml"),
			strings.HasSuffix(path, ".yml"),
			strings.HasSuffix(path, ".json"),
			strings.HasSuffix(path, ".ps1"):
		default:
			return nil
		}

		b, rerr := os.ReadFile(path) //nolint:gosec // repo-relative path from a walk
		if rerr != nil {
			return nil
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range arnAccount.FindAllStringSubmatch(line, -1) {
				acct := m[1]
				if placeholder.MatchString(acct) {
					continue
				}
				if _, ok := allowed[acct]; ok {
					continue
				}
				if acct == docExampleAccount && strings.HasSuffix(path, ".go") {
					continue
				}
				bad = append(bad, hit{file: rel, account: acct, line: strings.TrimSpace(line), n: i + 1})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if scanned == 0 {
		// A discovery that finds nothing has broken; it has not proven the repo
		// clean. Same rule as scripts/nested-modules.sh.
		t.Fatal("scanned no files — the discovery is broken, not the repo")
	}

	for _, h := range bad {
		t.Errorf("%s:%d names account %s, which this project does not own:\n    %s\n"+
			"Allowed accounts: %s\n"+
			"If this is a legitimate new spore.host account, add it to `allowed` in this test "+
			"with what it is for. If it is a cross-account grant into someone else's account, it "+
			"must not be committed.",
			h.file, h.n, h.account, h.line, strings.Join(sortedAccounts(allowed), ", "))
	}
}

func sortedAccounts(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+" ("+v+")")
	}
	sort.Strings(out)
	return out
}
