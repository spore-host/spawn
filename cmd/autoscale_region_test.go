package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAutoscaleHonoursTheRegionFlag guards spawn#774.
//
// `spawn autoscale` advertised the root --region flag — whose help says it
// "overrides SPORE_REGION/AWS_REGION and the shared config" — and ignored it.
// Both call sites built their AWS config with a bare
// config.LoadDefaultConfig(ctx), so the region came from the ambient chain and
// the flag did nothing. The flag was ignored in BOTH directions: AWS_REGION won
// even when --region contradicted it.
//
// Symptoms were a DynamoDB scan failing with ResourceNotFoundException against a
// region with no groups table, triggerLambda invoking a function by name in a
// region where it may not exist, and the #772 coverage check confidently
// reporting "no autoscale orchestrator runs in this account" against an account
// that has one.
//
// A source assertion rather than a behavioural one, deliberately: proving a
// region reached the SDK needs either a live account or an injected client, and
// neither exists on this path today. What is cheap and sufficient is forbidding
// the construct that caused it — an unparameterised LoadDefaultConfig in the
// autoscale call sites, which must go through autoscaleConfig instead.
func TestAutoscaleHonoursTheRegionFlag(t *testing.T) {
	// Matches a LoadDefaultConfig call with NO options — the broken form.
	// `config.LoadDefaultConfig(ctx, config.WithRegion(r))` is fine and does not
	// match, which is what distinguishes this from a blanket ban.
	bare := regexp.MustCompile(`config\.LoadDefaultConfig\(\s*ctx\s*\)`)

	files, err := filepath.Glob("autoscale*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no autoscale*.go files found — this gate is matching nothing")
	}

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// Strip comments so the explanation above (which names the broken call)
		// does not satisfy the matcher. Three gates this session passed against
		// their own prose; this one is written knowing that.
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if bare.MatchString(line) {
				t.Errorf("%s: config.LoadDefaultConfig(ctx) with no options ignores the --region "+
					"flag and the shared profile (spawn#774). Use autoscaleConfig(ctx), which "+
					"routes through aws.NewClientWithRegion(ctx, spawnRegion).\n  %s",
					f, strings.TrimSpace(line))
			}
		}
	}
}
