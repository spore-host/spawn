package cmd

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/schedulesweep/schedulesweeptest"
)

func runScheduleParamsCleanup(t *testing.T, s *schedulesweeptest.Scenario, dryRun bool) (string, error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	savedDry, savedYes := cleanupDryRun, cleanupYes
	t.Cleanup(func() { cleanupDryRun, cleanupYes = savedDry, savedYes })
	cleanupDryRun, cleanupYes = dryRun, true
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetErr(&out)
	err := runCleanupScheduleParams(context.Background(), c, schedulesweeptest.Account, s.Clients(), s.Now)
	return out.String(), err
}

// `spawn cleanup --schedule-params` deletes exactly what the shared scenario
// says (the reaper's TestScheduleParamsSweepAgreesWithTheCLI runs the same
// one): the two orphans, and none of the params a record names, the young
// ones, the queue configs (current and pre-#1399), the untagged or
// mismatched ones, or anything in a misnamed bucket. --dry-run changes
// nothing and names exactly those.
func TestCleanupScheduleParams(t *testing.T) {
	dry := schedulesweeptest.New()
	out, err := runScheduleParamsCleanup(t, dry, true)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(dry.S3.Deleted) != 0 {
		t.Errorf("--dry-run deleted %v", dry.S3.Deleted)
	}
	want := len(schedulesweeptest.WantDeleted)
	if !strings.Contains(out, "Dry run: "+strconv.Itoa(want)+" orphaned schedule params object(s) would be deleted") {
		t.Errorf("dry run did not say %d would go:\n%s", want, out)
	}
	for _, k := range schedulesweeptest.WantDeleted {
		if !strings.Contains(out, "s3://"+k+": delete") {
			t.Errorf("dry run does not name %s:\n%s", k, out)
		}
	}

	s := schedulesweeptest.New()
	out, err = runScheduleParamsCleanup(t, s, false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := append([]string(nil), s.S3.Deleted...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(schedulesweeptest.WantDeleted, ",") {
		t.Errorf("deleted %v, want %v\n%s", got, schedulesweeptest.WantDeleted, out)
	}
	if len(s.S3.BadOwner) != 0 || len(s.S3.WrongRegion) != 0 {
		t.Errorf("S3 calls without the account as owner %v, or in the wrong region %v", s.S3.BadOwner, s.S3.WrongRegion)
	}
}

// An unreadable schedules table: the command fails and deletes nothing.
func TestCleanupScheduleParamsRefusesWithoutTheTable(t *testing.T) {
	s := schedulesweeptest.New()
	s.DB.Err = errors.New("ResourceNotFoundException")
	out, err := runScheduleParamsCleanup(t, s, false)
	if err == nil || !strings.Contains(err.Error(), "no schedule params were deleted") {
		t.Errorf("err = %v, want a refusal\n%s", err, out)
	}
	if len(s.S3.Deleted) != 0 {
		t.Errorf("deleted %v with the table unreadable", s.S3.Deleted)
	}
}
