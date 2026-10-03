package cmd

import (
	"strings"
	"testing"
)

// withCostLimitFlags sets the package-level launch flags this check reads, and
// restores them. The flags are globals (cobra binds them that way), so a test that
// forgets to restore leaks into the next one.
func withCostLimitFlags(t *testing.T, limit float64, allowOverrun, create bool, capacity, throughput int32, lifecycle string) {
	t.Helper()
	oCL, oAllow, oCreate := costLimit, allowCostLimitOverrun, fsxCreate
	oCap, oThr, oLife := fsxStorageCapacity, fsxThroughput, fsxLifecycle
	costLimit, allowCostLimitOverrun, fsxCreate = limit, allowOverrun, create
	fsxStorageCapacity, fsxThroughput, fsxLifecycle = capacity, throughput, lifecycle
	t.Cleanup(func() {
		costLimit, allowCostLimitOverrun, fsxCreate = oCL, oAllow, oCreate
		fsxStorageCapacity, fsxThroughput, fsxLifecycle = oCap, oThr, oLife
	})
}

// TestCostLimitRefusesTheExact613Launch is spawn#616 against the launch that produced
// #613: `--cost-limit 2.50` alongside `--fsx-create`, which committed ~$174/month —
// roughly 70x the cap — while the cap was respected to the letter the whole time,
// because it only ever counted compute.
func TestCostLimitRefusesTheExact613Launch(t *testing.T) {
	withCostLimitFlags(t, 2.50, false, true, 1200, 125, "ephemeral")

	err := costLimitPreflight()
	if err == nil {
		t.Fatal("a launch committing ~$174/month of storage under a $2.50 cap must be refused")
	}
	msg := err.Error()
	for _, want := range []string{
		"$174",                       // the real number the reporter never saw
		"2.50",                       // the cap it exceeds
		"1200 GiB",                   // what is being created
		"ephemeral",                  // its lifecycle
		"outlives the instance",      // WHY the cap cannot bound it
		"--allow-cost-limit-overrun", // the escape hatch
		"--fsx-storage-capacity",     // and the cheaper alternative
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must mention %q:\n%s", want, msg)
		}
	}
}

func TestCostLimitAllowsWhenTheCapCoversTheStorage(t *testing.T) {
	// $200 comfortably covers a 1200 GiB filesystem's ~$174/month.
	withCostLimitFlags(t, 200, false, true, 1200, 125, "durable")
	if err := costLimitPreflight(); err != nil {
		t.Errorf("a cap that covers the commitment must not refuse: %v", err)
	}
}

// TestCostLimitOverrunFlagIsRespected: refusing without an escape hatch would block
// someone who knowingly wants a large durable filesystem under a small compute cap.
func TestCostLimitOverrunFlagIsRespected(t *testing.T) {
	withCostLimitFlags(t, 2.50, true, true, 1200, 125, "ephemeral")
	if err := costLimitPreflight(); err != nil {
		t.Errorf("--allow-cost-limit-overrun must let the launch proceed: %v", err)
	}
}

// TestCostLimitSilentWithoutACap: the check must do nothing when the user asked for no
// cap. Refusing an uncapped launch would be inventing a policy they didn't set.
func TestCostLimitSilentWithoutACap(t *testing.T) {
	withCostLimitFlags(t, 0, false, true, 1200, 125, "ephemeral")
	if err := costLimitPreflight(); err != nil {
		t.Errorf("no --cost-limit means no ceiling to exceed: %v", err)
	}
}

// TestCostLimitSilentWithoutStorage: a compute-only launch has no commitment the cap
// cannot bound, so it must pass however small the cap is — otherwise every small-cap
// launch breaks and the change is a regression rather than a fix.
func TestCostLimitSilentWithoutStorage(t *testing.T) {
	withCostLimitFlags(t, 0.01, false, false, 0, 0, "")
	if err := costLimitPreflight(); err != nil {
		t.Errorf("a launch creating no outliving storage must not be refused: %v", err)
	}
}

// TestStorageCommitmentUsesTheCreatePathDefaults: the quoted figure has to match what
// would actually be provisioned. Quoting a shape the launch wouldn't use is how
// #533's cap came to be enforced against a number nobody could reconcile.
func TestStorageCommitmentUsesTheCreatePathDefaults(t *testing.T) {
	// Zero capacity/throughput mean "the create path's defaults" (1200 GiB, 125).
	withCostLimitFlags(t, 1000, false, true, 0, 0, "ephemeral")
	c := launchStorageCommitment()

	if c.Empty() {
		t.Fatal("--fsx-create must produce a commitment even with no explicit size")
	}
	if c.MonthlyUSD < 173 || c.MonthlyUSD > 175 {
		t.Errorf("MonthlyUSD = %.2f, want ~174 (1200 GiB minimum at the 125 MB/s/TiB default)", c.MonthlyUSD)
	}
	if !strings.Contains(strings.Join(c.Items, " "), "1200 GiB") {
		t.Errorf("the item should name the defaulted capacity: %v", c.Items)
	}
}

// TestStorageCommitmentExcludesRootEBS. Root/data EBS carries DeleteOnTermination, so
// it dies with the instance and spored accounts for it in flight. Counting it here too
// would double-count and refuse launches that are genuinely fine.
func TestStorageCommitmentExcludesRootEBS(t *testing.T) {
	withCostLimitFlags(t, 1000, false, false, 0, 0, "")
	if c := launchStorageCommitment(); !c.Empty() {
		t.Errorf("a launch with no --fsx-create commits no outliving storage, got %v", c.Items)
	}
}

// TestCostLimitScopeNoteStatesTheSplit: the number is shown in the launch preview, and
// the honest thing to say beside it is which half the cap can actually act on.
func TestCostLimitScopeNoteStatesTheSplit(t *testing.T) {
	withCostLimitFlags(t, 50, false, false, 0, 0, "")
	note := costLimitScopeNote()
	for _, want := range []string{"compute + storage", "reaper"} {
		if !strings.Contains(note, want) {
			t.Errorf("scope note must contain %q, got: %s", want, note)
		}
	}

	withCostLimitFlags(t, 0, false, false, 0, 0, "")
	if costLimitScopeNote() != "" {
		t.Error("no cap means nothing to say about its scope")
	}
}
