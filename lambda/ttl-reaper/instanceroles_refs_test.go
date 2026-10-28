package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/instancerole/instanceroletest"
)

// fakeRefs is a ReferenceSource with a fixed answer or failure.
type fakeRefs struct {
	refs map[string][]string
	err  error
}

func (f *fakeRefs) Describe() string { return "schedules (test)" }

func (f *fakeRefs) ProfileReferences(context.Context) (map[string][]string, error) {
	return f.refs, f.err
}

// roleScenarioEC2 answers the instance scan (tag-filtered) with nothing and the
// role sweep's state-only DescribeInstances from the scenario.
type roleScenarioEC2 struct {
	emptyEC2
	s *instanceroletest.EC2
}

func (e roleScenarioEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, o ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	for _, flt := range in.Filters {
		if strings.HasPrefix(aws.ToString(flt.Name), "tag:") {
			return &ec2.DescribeInstancesOutput{}, nil
		}
	}
	return e.s.DescribeInstances(ctx, in, o...)
}

// scenarioReaper is the shared #1378 scenario as one reaper account, with the
// schedules read through the same instancerole.ScheduleSource configure builds.
func scenarioReaper(mode string) (*reaper, *instanceroletest.Scenario) {
	s := instanceroletest.New()
	r := &reaper{
		regions:       []string{instanceroletest.Region},
		maxAge:        defaultMaxAge,
		netResources:  netResourcesOff,
		spotRequests:  spotRequestsOff,
		elasticIPs:    elasticIPsOff,
		instanceRoles: mode,
		roleRefs:      []instancerole.ReferenceSource{s.Source()},
		accounts: []account{{
			label: "acct", accountID: instanceroletest.Account,
			ec2For: func(region string) ec2API { return roleScenarioEC2{s: s.EC2[region]} },
			iamFor: func() iamAPI { return s.IAM },
		}},
	}
	r.accounts[0].regionsFor = func() regionsAPI { return &fakeRegions{regions: s.Regions()} }
	return r, s
}

// Gate (#1378): the reaper deletes exactly what the shared scenario says, the
// same answer `spawn cleanup --instance-roles` gives on it
// (TestCleanupInstanceRolesAgreesWithTheReaper), and in report mode names the
// roles an enabled schedule keeps: "kept: referenced by schedule <id>".
func TestInstanceRoleSweepAgreesWithTheCLI(t *testing.T) {
	r, s := scenarioReaper(instanceRolesReap)
	var sum Summary
	out := captureLogs(func() { sum, _ = r.run(context.Background()) })
	if got, want := instanceroletest.Joined(s.IAM.DeletedSorted()), instanceroletest.Joined(instanceroletest.WantDeleted); got != want {
		t.Errorf("reap deleted %s, want %s\n%s", got, want, out)
	}

	r, s = scenarioReaper(instanceRolesReport)
	out = captureLogs(func() { sum, _ = r.run(context.Background()) })
	if len(s.IAM.Mutated) != 0 {
		t.Errorf("report changed IAM: %q", s.IAM.Mutated)
	}
	var would []string
	for _, m := range regexp.MustCompile(`WOULD delete role (\S+)`).FindAllStringSubmatch(out, -1) {
		would = append(would, m[1])
	}
	if got, want := instanceroletest.Joined(would), instanceroletest.Joined(instanceroletest.WantDeleted); got != want {
		t.Errorf("report would delete %s, want %s\n%s", got, want, out)
	}
	for role, why := range instanceroletest.WantReferenced {
		if line := "role " + role + " kept: " + why; !strings.Contains(out, line) {
			t.Errorf("report does not say %q:\n%s", line, out)
		}
	}
	if sum.InstanceRolesReferenced != len(instanceroletest.WantReferenced) {
		t.Errorf("instance_roles_referenced = %d, want %d", sum.InstanceRolesReferenced, len(instanceroletest.WantReferenced))
	}
}

// Gate (#1378): schedules that cannot be read completely mean no role in the
// account is even listed, and the account is counted. AccessDenied (the
// function role predating the grant) is counted apart from Errors; anything
// else, a missing params object included, is an error too.
func TestInstanceRoleSweepFailsClosedOnUnreadableSchedules(t *testing.T) {
	for _, c := range []struct {
		name       string
		break_     func(*instanceroletest.Scenario)
		wantErrors bool
	}{
		{"access denied", func(s *instanceroletest.Scenario) {
			s.Schedules.Err = apiErr("AccessDeniedException")
		}, false},
		{"throttled", func(s *instanceroletest.Scenario) {
			s.Schedules.Err = errors.New("ProvisionedThroughputExceededException")
		}, true},
		{"missing params object", func(s *instanceroletest.Scenario) {
			for k := range s.Schedules.Objects {
				if strings.Contains(k, "sched-row") {
					delete(s.Schedules.Objects, k)
				}
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, s := scenarioReaper(instanceRolesReap)
			c.break_(s)
			var sum Summary
			out := captureLogs(func() { sum, _ = r.run(context.Background()) })
			if len(s.IAM.Mutated) != 0 || len(s.IAM.Deleted) != 0 {
				t.Errorf("deleted roles without knowing which schedules name them: %q", s.IAM.Mutated)
			}
			if sum.InstanceRolesScanned != 0 {
				t.Errorf("listed %d roles without the schedules", sum.InstanceRolesScanned)
			}
			if sum.InstanceRolesReferencesUnreadable != 1 {
				t.Errorf("instance_roles_references_unreadable = %d, want 1", sum.InstanceRolesReferencesUnreadable)
			}
			if got := sum.Errors > 0; got != c.wantErrors {
				t.Errorf("errors = %d, want errors=%t", sum.Errors, c.wantErrors)
			}
			if !strings.Contains(out, "not swept, no role deleted") {
				t.Errorf("no log line saying why:\n%s", out)
			}
		})
	}
}

// No sources configured is a failed collection too: a reaper built without
// the schedules deletes nothing.
func TestInstanceRoleSweepWithoutReferenceSourcesDeletesNothing(t *testing.T) {
	r, s := scenarioReaper(instanceRolesReap)
	r.roleRefs = nil
	var sum Summary
	captureLogs(func() { sum, _ = r.run(context.Background()) })
	if len(s.IAM.Deleted) != 0 || sum.InstanceRolesReferencesUnreadable != 1 {
		t.Errorf("deleted %q with no reference sources (unreadable=%d)", s.IAM.Deleted, sum.InstanceRolesReferencesUnreadable)
	}
}
