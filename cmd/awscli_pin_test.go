package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/launcher"
)

// The release workflow verifies what this prints (#1515); it must be exactly
// the pins the bootstrap installs, one whitespace-separated line each.
func TestAWSCLIPinCommandPrintsThePins(t *testing.T) {
	var out bytes.Buffer
	awsCLIPinCmd.SetOut(&out)
	t.Cleanup(func() { awsCLIPinCmd.SetOut(nil) })
	if err := awsCLIPinCmd.RunE(awsCLIPinCmd, nil); err != nil {
		t.Fatal(err)
	}
	pins := launcher.AWSCLIPins()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(pins) == 0 || len(lines) != len(pins) {
		t.Fatalf("spawn awscli-pin printed %d lines for %d pins:\n%s", len(lines), len(pins), out.String())
	}
	for i, p := range pins {
		if got := strings.Fields(lines[i]); strings.Join(got, " ") != p.Arch+" "+p.Version+" "+p.SHA256 || len(got) != 3 {
			t.Errorf("line %d = %q, want %+v as <arch> <version> <sha256>", i, lines[i], p)
		}
	}
	if !awsCLIPinCmd.Hidden {
		t.Error("awscli-pin is release tooling and should stay hidden")
	}
}
