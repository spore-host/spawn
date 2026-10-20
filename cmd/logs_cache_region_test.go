package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// stubLookup replaces the ListInstances call behind instance lookups, records
// the region each call searched, and answers with instances.
func stubLookup(t *testing.T, instances []aws.InstanceInfo) *[]string {
	t.Helper()
	var regions []string
	real := listInstancesForLookup
	listInstancesForLookup = func(_ context.Context, _ *aws.Client, region, _ string) ([]aws.InstanceInfo, error) {
		regions = append(regions, region)
		return instances, nil
	}
	t.Cleanup(func() { listInstancesForLookup = real })
	return &regions
}

func setGlobalRegion(t *testing.T, r string) {
	t.Helper()
	saved := spawnRegion
	spawnRegion = r
	t.Cleanup(func() { spawnRegion = saved })
}

// #1160: the cache exists to keep a log after AWS forgets the instance, so it
// must be reachable — by name and by ID — once EC2 lists nothing at all.
func TestLogsCacheIsReachableAfterEC2ForgetsTheInstance(t *testing.T) {
	setTestHome(t, t.TempDir())
	stubLookup(t, nil) // EC2 has forgotten everything
	savedNoCache, savedConsole := logsNoCache, logsConsole
	logsNoCache, logsConsole = false, false
	t.Cleanup(func() { logsNoCache, logsConsole = savedNoCache, savedConsole })

	raw := "boot noise\n" + taskproto.ConsoleLogStartPrefix + " (last 50 lines) — workload failed ===\n" +
		"Traceback: the real reason\n" + taskproto.ConsoleLogEndMarker + "\n"
	writeConsoleCache("i-0123456789abcdef0", "my-job", raw)

	for _, ident := range []string{"my-job", "i-0123456789abcdef0"} {
		t.Run(ident, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := showTerminatedLog(context.Background(), nil, ident, &out, &errOut); err != nil {
				t.Fatalf("showTerminatedLog(%q) failed although the dump is cached: %v", ident, err)
			}
			if !strings.Contains(out.String(), "Traceback: the real reason") {
				t.Errorf("cached command log not printed; stdout=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}

	var out, errOut bytes.Buffer
	if err := showTerminatedLog(context.Background(), nil, "never-cached", &out, &errOut); err == nil {
		t.Error("an identifier with no instance and no cache entry should still be an error")
	}
}

// Among several cached instances that shared a name, the newest dump wins.
func TestLogsCacheByNamePicksTheNewest(t *testing.T) {
	setTestHome(t, t.TempDir())
	writeConsoleCache("i-old", "worker", "old")
	writeConsoleCache("i-new", "worker", "new")
	// Make the order explicit rather than relying on write timing.
	oldPath, newPath := consoleCachePath("i-old"), consoleCachePath("i-new")
	touchAt(t, oldPath, -2)
	touchAt(t, newPath, -1)
	id, raw, ok := cachedConsoleFor("worker")
	if !ok || id != "i-new" || raw != "new" {
		t.Errorf("cachedConsoleFor(worker) = %q, %q, %v; want i-new", id, raw, ok)
	}
}

// #1160: --region must narrow instance lookups to that region; unset, they
// search every region (ListInstances's "" contract).
func TestInstanceLookupsHonourTheGlobalRegion(t *testing.T) {
	for _, tc := range []struct{ flag, want string }{{"eu-west-2", "eu-west-2"}, {"", ""}} {
		t.Run("region="+tc.flag, func(t *testing.T) {
			setGlobalRegion(t, tc.flag)
			regions := stubLookup(t, nil)

			_, _ = resolveInstance(context.Background(), nil, "i-0abc")     // ID path, incl. the recently-gone hint
			_, _ = resolveInstance(context.Background(), nil, "some-name")  // name path
			_, _ = findRecentlyGone(context.Background(), nil, "some-name") // logs' terminated lookup

			if len(*regions) < 3 {
				t.Fatalf("expected at least 3 lookups through the seam, saw %d — the gate is not observing them", len(*regions))
			}
			for i, r := range *regions {
				if r != tc.want {
					t.Errorf("lookup %d searched region %q, want %q", i, r, tc.want)
				}
			}
		})
	}
}

func touchAt(t *testing.T, path string, hoursAgo int) {
	t.Helper()
	ts := time.Now().Add(time.Duration(hoursAgo) * time.Hour)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// A denied GetConsoleOutput must name the permission: it is outside the
// minimal IAM baseline, so a least-privilege user hits exactly this (#1160).
func TestConsoleReadDenialNamesThePermission(t *testing.T) {
	denied := fmt.Errorf("get console output for i-1: %w", &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "nope"})
	if got := consoleReadError(denied).Error(); !strings.Contains(got, "ec2:GetConsoleOutput") {
		t.Errorf("denial does not name ec2:GetConsoleOutput: %s", got)
	}
	other := consoleReadError(errors.New("throttled")).Error()
	if strings.Contains(other, "ec2:GetConsoleOutput") {
		t.Errorf("a non-denial blames the permission: %s", other)
	}
}
