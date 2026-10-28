package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/instancerole/instanceroletest"
)

func runScenarioRolesCleanup(t *testing.T, s *instanceroletest.Scenario, dryRun bool) (string, error) {
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
	err := runCleanupInstanceRoles(context.Background(), c, s.IAM, s.EC2Clients(),
		[]instancerole.ReferenceSource{s.Source()}, s.Now)
	return out.String(), err
}

// Gate (#1378): `spawn cleanup --instance-roles` deletes exactly what the
// shared scenario says, the same answer the reaper gives on it
// (TestInstanceRoleSweepAgreesWithTheCLI), and its preview says why each
// schedule-named role is kept.
func TestCleanupInstanceRolesAgreesWithTheReaper(t *testing.T) {
	dry := instanceroletest.New()
	out, err := runScenarioRolesCleanup(t, dry, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.IAM.Mutated) != 0 {
		t.Errorf("--dry-run changed IAM: %q", dry.IAM.Mutated)
	}
	for role, why := range instanceroletest.WantReferenced {
		found := false
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, role) && strings.Contains(line, "keep") && strings.Contains(line, why) {
				found = true
			}
		}
		if !found {
			t.Errorf("preview does not keep %s as %q:\n%s", role, why, out)
		}
	}

	s := instanceroletest.New()
	if out, err = runScenarioRolesCleanup(t, s, false); err != nil {
		t.Fatal(err)
	}
	if got, want := instanceroletest.Joined(s.IAM.DeletedSorted()), instanceroletest.Joined(instanceroletest.WantDeleted); got != want {
		t.Errorf("cleanup deleted %s, want %s\n%s", got, want, out)
	}
}

// Gate (#1378): if the schedules cannot be read, the CLI deletes nothing and
// says so.
func TestCleanupInstanceRolesFailsClosedOnUnreadableSchedules(t *testing.T) {
	for name, brk := range map[string]func(*instanceroletest.Scenario){
		"access denied": func(s *instanceroletest.Scenario) {
			s.Schedules.Err = errors.New("AccessDeniedException: not authorized to perform dynamodb:Scan")
		},
		"missing params object": func(s *instanceroletest.Scenario) {
			for k := range s.Schedules.Objects {
				if strings.Contains(k, "sched-defaults") {
					delete(s.Schedules.Objects, k)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := instanceroletest.New()
			brk(s)
			_, err := runScenarioRolesCleanup(t, s, false)
			if err == nil || !strings.Contains(err.Error(), "no IAM role was deleted") {
				t.Errorf("unreadable schedules: err %v, want a refusal saying no role was deleted", err)
			}
			if len(s.IAM.Mutated) != 0 {
				t.Errorf("deleted roles without the schedules: %q", s.IAM.Mutated)
			}
		})
	}
}
