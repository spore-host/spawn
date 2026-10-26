package aws

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/aws/smithy-go/middleware"

	"github.com/spore-host/spawn/pkg/tagprefix"
	"github.com/spore-host/spawn/pkg/testutil"
)

// These are the #1168 gates for the group `spawn launch --efa --efa-group NAME`
// creates or joins.

func describeOneSG(t *testing.T, c *Client, sgID string) types.SecurityGroup {
	t.Helper()
	out, err := c.regionalEC2("us-east-1").DescribeSecurityGroups(context.Background(),
		&ec2.DescribeSecurityGroupsInput{GroupIds: []string{sgID}})
	if err != nil || len(out.SecurityGroups) != 1 {
		t.Fatalf("describe %s: %v (%d groups)", sgID, err, len(out.SecurityGroups))
	}
	return out.SecurityGroups[0]
}

func createEFAGroup(t *testing.T, c *Client, vpcID, name string) string {
	t.Helper()
	id, err := c.CreateOrGetEFASecurityGroup(context.Background(), "us-east-1", vpcID, name)
	if err != nil {
		if strings.Contains(err.Error(), "InvalidAction") {
			t.Skipf("emulator serves no security-group API: %v", err)
		}
		t.Fatalf("CreateOrGetEFASecurityGroup: %v", err)
	}
	return id
}

// EFA needs all traffic from the group itself, in and out (the issue's ask),
// and separate launches must land in ONE group, so a second call joins it.
func TestEFAGroupAdmitsAllTrafficFromItselfAndIsShared(t *testing.T) {
	c, vpcID := mpiTestClient(t)
	id := createEFAGroup(t, c, vpcID, "spawn-efa-fleet-1168")

	if p := selfReferentialProtocols(t, c, id, false); !containsProto(p, "-1") {
		t.Errorf("self-referential ingress = %v, want \"-1\": EFA's SRD is not TCP", p)
	}
	if p := selfReferentialProtocols(t, c, id, true); !containsProto(p, "-1") {
		t.Errorf("self-referential egress = %v, want \"-1\": AWS documents EFA as needing it", p)
	}

	again := createEFAGroup(t, c, vpcID, "spawn-efa-fleet-1168")
	if again != id {
		t.Errorf("second launch got group %s, want the first launch's %s: the fleet would be split "+
			"across two groups and EFA between them would fail", again, id)
	}
}

// An existing group is brought up to date, as the MPI group is (#659).
func TestEFAGroupBackfillsAnExistingGroup(t *testing.T) {
	c, vpcID := mpiTestClient(t)
	created, err := c.regionalEC2("us-east-1").CreateSecurityGroup(context.Background(), &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("spawn-efa-handmade"), Description: aws.String("bare"), VpcId: aws.String(vpcID),
	})
	if err != nil {
		if strings.Contains(err.Error(), "InvalidAction") {
			t.Skipf("emulator serves no security-group API: %v", err)
		}
		t.Fatal(err)
	}
	if p := selfReferentialProtocols(t, c, *created.GroupId, false); containsProto(p, "-1") {
		t.Fatalf("fixture already allows all traffic (%v)", p)
	}
	if got := createEFAGroup(t, c, vpcID, "spawn-efa-handmade"); got != *created.GroupId {
		t.Fatalf("got %s, want the existing %s", got, *created.GroupId)
	}
	if p := selfReferentialProtocols(t, c, *created.GroupId, false); !containsProto(p, "-1") {
		t.Errorf("after joining, self ingress = %v, want \"-1\" added", p)
	}
}

// The deleter (owner principle: name it) is the reaper's managed-network sweep,
// lambda/ttl-reaper reapSecurityGroups. It selects groups by
// tag:spawn:managed=true and ages them by spawn:created (RFC 3339). A group
// missing either is never reaped on time.
//
// Read from the CreateSecurityGroup request spawn actually sends, captured in
// the SDK's middleware stack: the emulator neither stores security-group tags
// nor applies tag filters, so asking it back would prove nothing.
func TestEFAGroupCarriesWhatTheReaperSweepSelectsAndAges(t *testing.T) {
	env := testutil.SubstrateServer(t)
	var sent []*ec2.CreateSecurityGroupInput
	cfg := env.AWSConfig.Copy()
	cfg.APIOptions = append(cfg.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("captureCreateSG",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
				if p, ok := in.Parameters.(*ec2.CreateSecurityGroupInput); ok {
					sent = append(sent, p)
				}
				return next.HandleInitialize(ctx, in)
			}), middleware.Before)
	})
	c := NewClientFromConfig(cfg)
	vpc, err := c.regionalEC2("us-east-1").CreateVpc(context.Background(), &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	if err != nil {
		t.Fatalf("create test VPC: %v", err)
	}
	createEFAGroup(t, c, *vpc.Vpc.VpcId, "spawn-efa-reaped-1168")

	if len(sent) != 1 {
		t.Fatalf("captured %d CreateSecurityGroup calls, want 1", len(sent))
	}
	tags := map[string]string{}
	for _, spec := range sent[0].TagSpecifications {
		if spec.ResourceType != types.ResourceTypeSecurityGroup {
			continue
		}
		for _, tg := range spec.Tags {
			tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
	}
	if tags[tagprefix.Tag("managed")] != "true" {
		t.Errorf("%s = %q, want \"true\": the reaper sweep selects on it, so the group would never be deleted",
			tagprefix.Tag("managed"), tags[tagprefix.Tag("managed")])
	}
	if ts, err := time.Parse(time.RFC3339, tags[tagprefix.Tag("created")]); err != nil || time.Since(ts) > time.Hour {
		t.Errorf("%s = %q, want an RFC 3339 creation time: without it the sweep waits its 30-day untagged grace",
			tagprefix.Tag("created"), tags[tagprefix.Tag("created")])
	}
	if tags[tagprefix.Tag("purpose")] != EFAGroupPurpose {
		t.Errorf("%s = %q, want %q", tagprefix.Tag("purpose"), tags[tagprefix.Tag("purpose")], EFAGroupPurpose)
	}
}

// No SSH from the world: joining an EFA group leaves outside reachability as a
// plain launch has it. Only the self rule is added.
func TestEFAGroupOpensNothingOutside(t *testing.T) {
	c, vpcID := mpiTestClient(t)
	sg := describeOneSG(t, c, createEFAGroup(t, c, vpcID, "spawn-efa-closed-1168"))
	for _, p := range sg.IpPermissions {
		if len(p.IpRanges) > 0 || len(p.Ipv6Ranges) > 0 || len(p.PrefixListIds) > 0 {
			t.Errorf("EFA group has an outside ingress rule %+v; it should admit only its own members", p)
		}
	}
}

// The #685 guard holds for the EFA group too: "spawn-efa-" + "" must not become
// one group every unnamed launch shares.
func TestEFAGroupRefusesABarePrefix(t *testing.T) {
	c := &Client{}
	if _, err := c.CreateOrGetEFASecurityGroup(context.Background(), "us-east-1", "vpc-1", EFAGroupPrefix); err == nil {
		t.Fatal("bare spawn-efa- accepted")
	}
}
