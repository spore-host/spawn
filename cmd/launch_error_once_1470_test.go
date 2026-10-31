package cmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
)

// #1470: a single launch's failure is printed once. The launch step shows it
// (progress.Error) and returns it, and the root printer then printed it again
// plain, so a pinned-AZ capacity error read twice. Driven through the launch
// step and the root printer, the two writers of that text, into one buffer;
// in -o json mode the progress display prints nothing and the root printer
// still must.
func TestLaunchFailureIsPrintedOnce(t *testing.T) {
	// Each case its own zone: progress.Reported is per process, as a command
	// is, so the cases must not share an error text.
	for name, tc := range map[string]struct {
		prog *progress.Progress
		zone string
	}{
		"progress display": {progress.NewProgress(), "us-east-1f"},
		"-o json (quiet)":  {progress.NewQuietProgress(), "us-east-1e"},
	} {
		t.Run(name, func(t *testing.T) {
			prog := tc.prog
			launchErr := &aws.ZoneFallbackError{
				InstanceType: "c8gn.48xlarge", Region: "us-east-1",
				Attempts: []aws.ZoneAttempt{{Zone: tc.zone, Code: "InsufficientInstanceCapacity",
					Message: "We currently do not have sufficient c8gn.48xlarge capacity in the Availability Zone you requested (" + tc.zone + ")."}},
				Blocker:    "the launch is pinned to " + tc.zone + " (--az)",
				OtherZones: []string{"us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d"},
			}
			var out strings.Builder
			prog.SetErrorOutput(&out)
			launches := 0
			launch := func(context.Context, aws.LaunchConfig, io.Writer) (*aws.LaunchResult, error) {
				launches++
				return nil, launchErr
			}
			cfg := &aws.LaunchConfig{InstanceType: "c8gn.48xlarge", Region: "us-east-1", AvailabilityZone: tc.zone}
			_, err := launchInstanceStep(context.Background(), launch, cfg, prog, audit.NewLogger(io.Discard, "", ""), io.Discard)
			if launches != 1 || err == nil {
				t.Fatalf("launch step: %d launches, err %v; want one launch returning its error", launches, err)
			}
			printCommandError(&out, err)

			for _, line := range []string{
				"failed to launch instance: InsufficientInstanceCapacity in " + tc.zone,
				"spawn did not try another availability zone",
				"lagotto watch c8gn.48xlarge --regions us-east-1",
			} {
				if n := strings.Count(out.String(), line); n != 1 {
					t.Errorf("%q printed %d times, want once:\n%s", line, n, out.String())
				}
			}
		})
	}
}
