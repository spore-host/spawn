package main

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// The guard is tested where it lives (CLAUDE.md, "Writing a gate", rule 2), with
// the states its callers may never produce.
func TestGuardTeardownRefusesTheInfraAccountAndAnUnknownOne(t *testing.T) {
	for _, c := range []struct {
		account string
		want    error
	}{
		{"966362334030", ErrInfraAccount},
		{"", ErrAccountUnknown},
		{"435415984226", nil},
		{"111111111111", nil},
	} {
		err := GuardTeardown(c.account)
		if c.want == nil && err != nil {
			t.Errorf("GuardTeardown(%q) = %v, want nil", c.account, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("GuardTeardown(%q) = %v, want %v", c.account, err, c.want)
		}
	}
}

// The infra account's ID is the documented one, not whatever the constant says.
func TestInfraAccountIsTheDocumentedOne(t *testing.T) {
	if InfraAccountID != "966362334030" {
		t.Fatalf("InfraAccountID = %q; docs/infra-account.md names 966362334030", InfraAccountID)
	}
}

func TestGuardDeleteRefusesAServiceResourceInAnyAccount(t *testing.T) {
	service := map[string]string{"spawn:managed": "true", "spore-host:role": "service", "spore-host:component": "dashboard-api"}
	litter := map[string]string{"spawn:managed": "true"}
	for _, c := range []struct {
		name    string
		account string
		tags    map[string]string
		want    error
	}{
		{"service resource, member account", "435415984226", service, ErrServiceResource},
		{"service resource, infra account", "966362334030", service, ErrInfraAccount},
		{"litter, member account", "435415984226", litter, nil},
		{"litter, infra account", "966362334030", litter, ErrInfraAccount},
		{"litter, unknown account", "", litter, ErrAccountUnknown},
		{"untagged, member account", "435415984226", nil, nil},
		{"role tag with another value", "435415984226", map[string]string{"spore-host:role": "workload"}, nil},
	} {
		err := GuardDelete(c.account, "sg-1", c.tags)
		if c.want == nil && err != nil {
			t.Errorf("%s: GuardDelete = %v, want nil", c.name, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: GuardDelete = %v, want %v", c.name, err, c.want)
		}
	}
}

// The four orphan sweeps call GuardTeardown before they touch the account: in
// the infra account, or one the reaper could not identify, none of them makes a
// single call, in reap mode.
func TestSweepsMakeNoCallInTheInfraAccount(t *testing.T) {
	for _, id := range []string{InfraAccountID, ""} {
		touched := map[string]bool{}
		acct := account{
			label: "infra", accountID: id,
			netFor:     func(string) netResourceAPI { touched["net"] = true; return &fakeNetAPI{} },
			s3For:      func(string) s3API { touched["s3"] = true; return nil },
			iamFor:     func() iamAPI { touched["iam"] = true; return nil },
			sqsFor:     func(string) sqsAPI { touched["sqs"] = true; return nil },
			regionsFor: func() regionsAPI { touched["regions"] = true; return nil },
		}
		r := &reaper{
			netResources: netResourcesReap, elasticIPs: elasticIPsReap, emptyBuckets: emptyBucketsReap,
			instanceRoles: instanceRolesReap, poolQueues: poolQueuesReap, regions: []string{"us-east-1"},
		}
		sum := &Summary{}
		now := time.Now()
		r.reapNetResourcesRegion(context.Background(), acct, "us-east-1", now, sum, &accountOutcome{})
		r.reapEmptyBucketsRegion(context.Background(), acct, "us-east-1", now, sum)
		r.reapInstanceRolesAccount(context.Background(), acct, now, sum)
		r.reapPoolQueuesAccount(context.Background(), acct, now, sum)
		if len(touched) != 0 {
			t.Errorf("account %q: the sweeps reached %v; they must refuse before any call", id, touched)
		}
	}
}

// In a member account the net-resource sweep reaps litter but keeps a group or
// placement group tagged spore-host:role=service, however old and unused, and
// does not even stamp an untimed one.
func TestNetSweepKeepsServiceResources(t *testing.T) {
	now := time.Now()
	service := func(tags []ec2types.Tag) []ec2types.Tag {
		return append(tags, ec2types.Tag{Key: awssdk.String("spore-host:role"), Value: awssdk.String("service")})
	}
	oldSG := sg("sg-service", "spawn-mpi-x", netResourceGrace+time.Hour, now)
	oldSG.Tags = service(oldSG.Tags)
	untimedSG := ec2types.SecurityGroup{GroupId: awssdk.String("sg-service-untimed"), GroupName: awssdk.String("y"), Tags: service(nil)}
	oldPG := pgrp("pg-service", netResourceGrace+time.Hour, now)
	oldPG.Tags = service(oldPG.Tags)
	f := &fakeNetAPI{
		sgs: []ec2types.SecurityGroup{oldSG, untimedSG, sg("sg-litter", "spawn-mpi-z", netResourceGrace+time.Hour, now)},
		pgs: []ec2types.PlacementGroup{oldPG, pgrp("pg-litter", netResourceGrace+time.Hour, now)},
	}
	runNetSweep(t, f, false)
	if len(f.deletedSG) != 1 || f.deletedSG[0] != "sg-litter" {
		t.Errorf("deleted security groups = %v, want only sg-litter", f.deletedSG)
	}
	if len(f.deletedPG) != 1 || f.deletedPG[0] != "pg-litter" {
		t.Errorf("deleted placement groups = %v, want only pg-litter", f.deletedPG)
	}
	if len(f.tagged) != 0 {
		t.Errorf("stamped %v: a service resource is not aged at all", f.tagged)
	}
}
