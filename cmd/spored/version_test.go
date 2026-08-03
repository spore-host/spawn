package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionHasNoHardcodedDefault mirrors the spawn-side guard. spored's Version
// said "0.1.0" for 97 releases, and unlike spawn's the value is not merely
// cosmetic: spored writes it to the instance's spawn:spored-version tag, which
// `spawn status` and `spawn upgrade-spored` read to decide whether an in-place
// upgrade took effect. A stale constant there makes an upgrade look like a no-op.
func TestVersionHasNoHardcodedDefault(t *testing.T) {
	if Version != "" {
		t.Errorf("main.Version = %q; it must be empty in source. GoReleaser injects "+
			"the real value via -X main.Version, and non-release builds fall back to "+
			"the Go build stamp (pkg/buildinfo). A default here is what nobody "+
			"remembers to bump.", Version)
	}
}

// TestVersionSubcommandReportsResolvedVersion locks the output contract
// scripts/install-spored.sh parses ("spored version X"), and checks it reports
// the resolved version rather than the empty variable — an empty third field
// would make the install script's version comparison silently compare "".
func TestVersionSubcommandReportsResolvedVersion(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("spored version: %v", err)
	}

	// The subcommand prints with fmt.Printf (stdout, not the cobra writer), so
	// assert on the resolved value directly rather than on captured output.
	got := version()
	if strings.TrimSpace(got) == "" {
		t.Fatal("version() is empty; `spored version` would print a blank version")
	}
	if got == "0.1.0" {
		t.Error("version() = 0.1.0, the old hardcoded default")
	}
}
