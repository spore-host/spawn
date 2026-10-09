package aws

import "testing"

// TestMatchesFootprintNameFindsThePrefixlessOnes is the regression guard for the
// flaw the command had on its first real run.
//
// The prefix list could not see /aws/lambda/github-oauth-bridge — the orphaned
// log group that motivated the orphan check in the first place. A blind spot that
// hides its own motivating example is not acceptable, so the exact-name list
// exists and this pins it.
func TestMatchesFootprintNameFindsThePrefixlessOnes(t *testing.T) {
	for _, name := range []string{
		"/aws/lambda/github-oauth-bridge",
		"github-oauth-bridge",
		"scheduler-handler",
		"portal-phone-home",
		"prism-bot",
	} {
		if !matchesFootprintName(name) {
			t.Errorf("matchesFootprintName(%q) = false. This is a spore.host resource with no "+
				"name prefix; if it is not matched it is invisible to `spawn footprint`, which is "+
				"how the orphaned github-oauth-bridge log group escaped the first version.", name)
		}
	}
}

func TestMatchesFootprintNameAcceptsThePrefixes(t *testing.T) {
	for _, name := range []string{
		"spawn-ttl-reaper-production",
		"/aws/lambda/spawn-dashboard-api",
		"/aws/lambda/lagotto-capacity-poller",
		"spored-instance-role",
		"truffle-something",
		"spore-bot-role",
		"/aws/imagebuilder/spawn-win11", // the imagebuilder prefix is stripped too
	} {
		if !matchesFootprintName(name) {
			t.Errorf("matchesFootprintName(%q) = false, want true", name)
		}
	}
}

// TestMatchesFootprintNameRejectsOtherPeoplesResources is the half that protects
// someone else's data.
//
// `spawn footprint` is read-only, but it feeds judgements about what to delete,
// and an over-broad match would invite deleting an unrelated project's
// resources. The personal account that prompted #653's log-retention work holds
// geos-chem-*, inside-the-lines-web-tool and /aws/batch/job — none of which are
// spore.host's, and none of which spawn should claim.
func TestMatchesFootprintNameRejectsOtherPeoplesResources(t *testing.T) {
	for _, name := range []string{
		"/aws/lambda/geos-chem-cost-tracking-RealTimeCostTrackerFunctio-8eJNwZx0j0sf",
		"/aws/lambda/inside-the-lines-web-tool",
		"/aws/batch/job",
		"/aws/sagemaker/ProcessingJobs",
		"/aws/apigateway/welcome",
		"my-unrelated-bucket",
		// Near-misses: these must NOT match on a substring.
		"not-spawn-related",
		"legacy-spore",
	} {
		if matchesFootprintName(name) {
			t.Errorf("matchesFootprintName(%q) = true. Claiming someone else's resource is worse "+
				"than missing one of ours: this report feeds deletion decisions.", name)
		}
	}
}
