package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/infrastructure"
)

// A failing health check gets one actionable line, and that line must lead
// somewhere real.
//
// #790: `spawn validate --infrastructure` recommended
// `spawn config deploy-infrastructure`, which does not exist. Worse than an
// unknown-command error — `config` is an alias for `instance-config`, so the
// advice resolved to "read runtime config over SSH from an instance named
// deploy-infrastructure". It also recommended `spawn config init --self-hosted`;
// there is no `config init` either. Both had been there since the initial
// commit.
//
// This gate lives in cmd/ rather than next to the validator because it resolves
// commands against the real cobra tree, which is the only authority on what
// exists. Checking the first word is not enough: `config` IS a real command, and
// the defect was the subcommand after it.
func TestValidateRecommendationsNameOnlyRealCommandsAndPaths(t *testing.T) {
	recs := failingInfrastructureRecommendations(t)
	if len(recs) == 0 {
		// Refusing to pass vacuously: with every resource class failing there
		// must be advice, and a gate over an empty list checks nothing.
		t.Fatal("a fully failing validation produced no recommendations — the fixture is broken, not the code")
	}

	root := repoRootFromCmd(t)
	// `~` is inside the class on purpose: without it the match on
	// "~/.spawn/config.yaml" starts after the tilde and the home-directory skip
	// below never fires, so the gate demands a repo file called
	// "/.spawn/config.yaml". Caught by running it.
	pathRef := regexp.MustCompile(`[~A-Za-z0-9._/-]+\.(?:sh|yaml|yml|md)`)

	for _, rec := range recs {
		// Half one: `spawn` may appear only as a backtick-quoted command. Prose
		// like "from the spawn repo" is indistinguishable from an invocation to
		// any parser, and the original defect was written bare.
		for _, idx := range allIndexes(rec, "spawn ") {
			if insideBackticks(rec, idx) {
				continue
			}
			t.Errorf("recommendation mentions `spawn` outside backticks, so it cannot be told from prose:\n  %s", rec)
		}

		// Half two: every backtick-quoted command must resolve completely.
		for _, inv := range backtickedSpawnCommands(rec) {
			words := strings.Fields(inv)
			var args []string
			for _, w := range words[1:] { // drop "spawn"
				if strings.HasPrefix(w, "-") {
					break // flags are not part of the command path
				}
				args = append(args, w)
			}
			cmd, leftover, err := rootCmd.Find(args)
			if err != nil || len(leftover) > 0 {
				t.Errorf("recommendation names `%s`, which is not a command.\n"+
					"  closest match: %q, unresolved: %v\n  in: %s",
					inv, cmd.CommandPath(), leftover, rec)
			}
		}

		// Half three: every path named must exist.
		for _, p := range pathRef.FindAllString(rec, -1) {
			if strings.HasPrefix(p, "~") || strings.HasPrefix(p, ".spawn") {
				continue // a user's own config file, not a repo path
			}
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				t.Errorf("recommendation names path %q, which does not exist in the repo.\n  in: %s", p, rec)
			}
		}
	}
}

// Flags named in a recommendation must exist too — `--self-hosted` was advertised
// on a command that has no such flag.
func TestValidateRecommendationsNameOnlyRealFlags(t *testing.T) {
	recs := failingInfrastructureRecommendations(t)
	for _, rec := range recs {
		for _, inv := range backtickedSpawnCommands(rec) {
			words := strings.Fields(inv)
			var args, flags []string
			for _, w := range words[1:] {
				if strings.HasPrefix(w, "--") {
					flags = append(flags, strings.SplitN(strings.TrimPrefix(w, "--"), "=", 2)[0])
					continue
				}
				if len(flags) == 0 {
					args = append(args, w)
				}
			}
			if len(flags) == 0 {
				continue
			}
			cmd, _, err := rootCmd.Find(args)
			if err != nil {
				continue // the command half of the gate reports this
			}
			for _, f := range flags {
				if cmd.Flags().Lookup(f) == nil && cmd.InheritedFlags().Lookup(f) == nil {
					t.Errorf("recommendation names `--%s` on %q, which has no such flag.\n  in: %s",
						f, cmd.CommandPath(), rec)
				}
			}
		}
	}
}

// failingInfrastructureRecommendations drives GetRecommendations over EVERY
// branch and returns the union of the advice produced.
//
// Both shapes are required, and finding that out cost a round trip. The Lambda
// branch names the mechanism per function when it has resource detail and falls
// back to a single generic line when it does not. A fixture that populates
// Resources therefore never reaches the fallback string — so with only that
// fixture, restoring the original `spawn config deploy-infrastructure` in the
// fallback PASSED this gate. A gate that cannot reach the string it is guarding
// is the #787 shape: it distinguishes nothing.
func failingInfrastructureRecommendations(t *testing.T) []string {
	t.Helper()

	// Self-hosted so the mode-specific branch fires as well.
	resolver := infrastructure.NewResolver(
		&config.InfrastructureConfig{Mode: config.InfrastructureModeSelfHosted},
		"us-east-1", "123456789012")
	v := infrastructure.NewValidator(resolver, awssdk.Config{})

	errs := []string{
		`DynamoDB table "spawn-schedules" does not exist`,
		`S3 bucket "spawn-schedules-us-east-1" does not exist`,
		`Lambda function "scheduler-handler" does not exist`,
	}

	// All four functions, so every per-function string is exercised — including
	// the one that deliberately names no path.
	withDetail := &infrastructure.ValidationResult{
		Valid:  false,
		Errors: errs,
		Resources: map[string]infrastructure.ResourceStatus{
			"lambda_scheduler_handler": {
				Name: "arn:aws:lambda:us-east-1:966362334030:function:scheduler-handler",
				Type: "Lambda Function",
			},
			"lambda_sweep_orchestrator": {
				Name: "arn:aws:lambda:us-east-1:966362334030:function:spawn-sweep-orchestrator",
				Type: "Lambda Function",
			},
			"lambda_alert_handler": {
				Name: "arn:aws:lambda:us-east-1:966362334030:function:spawn-alert-handler",
				Type: "Lambda Function",
			},
			"lambda_dashboard_api": {
				Name: "arn:aws:lambda:us-east-1:966362334030:function:spawn-dashboard-api",
				Type: "Lambda Function",
			},
			"lambda_unknown": {
				Name: "arn:aws:lambda:us-east-1:966362334030:function:spawn-something-new",
				Type: "Lambda Function",
			},
		},
	}

	// Errors but no resource detail: the fallback branch.
	withoutDetail := &infrastructure.ValidationResult{
		Valid:     false,
		Errors:    errs,
		Resources: map[string]infrastructure.ResourceStatus{},
	}

	recs := v.GetRecommendations(withDetail)
	fallback := v.GetRecommendations(withoutDetail)
	if len(fallback) == 0 {
		t.Fatal("the no-resource-detail shape produced no recommendations; the fallback branch is unreachable")
	}
	return append(recs, fallback...)
}

func backtickedSpawnCommands(s string) []string {
	var out []string
	parts := strings.Split(s, "`")
	for i := 1; i < len(parts); i += 2 { // odd indexes are inside backticks
		if strings.HasPrefix(parts[i], "spawn ") {
			out = append(out, parts[i])
		}
	}
	return out
}

func insideBackticks(s string, idx int) bool {
	return strings.Count(s[:idx], "`")%2 == 1
}

func allIndexes(s, sub string) []int {
	var out []int
	for i := 0; ; {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			return out
		}
		out = append(out, i+j)
		i += j + 1
	}
}

func repoRootFromCmd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module github.com/spore-host/spawn\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the repo root go.mod")
	return ""
}
