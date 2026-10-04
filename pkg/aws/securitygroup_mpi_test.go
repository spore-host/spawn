package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/testutil"
)

// mpiTestClient returns a substrate-backed client and a VPC to build security
// groups in.
//
// The VPC is CREATED here rather than read via GetDefaultVPC: the emulator
// serves no default VPC, so asking for one skipped every test in this file —
// and a skipped test that reads as a pass is how #659 survived in the first
// place. Creating one makes the coverage real and independent of what the
// emulator happens to pre-populate.
func mpiTestClient(t *testing.T) (*Client, string) {
	t.Helper()
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	out, err := c.regionalEC2("us-east-1").CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	})
	if err != nil {
		t.Fatalf("create test VPC: %v\n\nWithout a VPC these tests cannot run, and "+
			"skipping them would leave #659 uncovered while looking green.", err)
	}
	return c, *out.Vpc.VpcId
}

// selfReferentialProtocols returns the IP protocols the group allows from
// ITSELF, which is the only thing that matters for intra-cluster traffic.
func selfReferentialProtocols(t *testing.T, c *Client, sgID string, egress bool) []string {
	t.Helper()
	out, err := c.regionalEC2("us-east-1").DescribeSecurityGroups(context.Background(),
		&ec2.DescribeSecurityGroupsInput{GroupIds: []string{sgID}})
	if err != nil {
		t.Fatalf("describe %s: %v", sgID, err)
	}
	if len(out.SecurityGroups) == 0 {
		t.Fatalf("security group %s not found after creation", sgID)
	}

	perms := out.SecurityGroups[0].IpPermissions
	if egress {
		perms = out.SecurityGroups[0].IpPermissionsEgress
	}

	var protos []string
	for _, p := range perms {
		for _, pair := range p.UserIdGroupPairs {
			if pair.GroupId != nil && *pair.GroupId == sgID {
				protos = append(protos, aws.ToString(p.IpProtocol))
			}
		}
	}
	return protos
}

// TestMPISecurityGroupAllowsAllProtocolsFromItself is spawn#659.
//
// The group authorized only `tcp` 0-65535 from itself. EFA's Scalable Reliable
// Datagram is NOT TCP, so the fabric could not pass traffic at all — and this is
// a hard failure, not a slowdown: GCHP/MAPL aborts at MPI_Win_create when the
// one-sided transport is unavailable, rather than falling back.
//
// EFA requires all-traffic self-referential rules, which means IpProtocol "-1".
func TestMPISecurityGroupAllowsAllProtocolsFromItself(t *testing.T) {
	c, vpcID := mpiTestClient(t)

	sgID, err := c.CreateOrGetMPISecurityGroup(context.Background(), "us-east-1", vpcID, "spawn-mpi-test-659")
	if err != nil {
		if strings.Contains(err.Error(), "InvalidAction") {
			t.Skipf("emulator serves no security-group API: %v", err)
		}
		t.Fatalf("CreateOrGetMPISecurityGroup: %v", err)
	}

	ingress := selfReferentialProtocols(t, c, sgID, false)
	if !containsProto(ingress, "-1") {
		t.Errorf("self-referential INGRESS protocols = %v, want \"-1\" (all traffic).\n\n"+
			"EFA's SRD is not TCP, so a tcp-only rule blocks the fabric entirely. "+
			"GCHP/MAPL aborts at MPI_Win_create rather than degrading, so this is a hard "+
			"failure for that class of code (#659).", ingress)
	}

	egress := selfReferentialProtocols(t, c, sgID, true)
	if !containsProto(egress, "-1") {
		t.Errorf("self-referential EGRESS protocols = %v, want \"-1\".\n\n"+
			"A new security group gets a default allow-all egress rule, which is why this "+
			"was not noticed — but EFA is documented as needing the self-referential "+
			"egress rule explicitly, and anyone who tightens the default rule loses the "+
			"fabric with no indication why.", egress)
	}
}

// TestMPISecurityGroupBackfillsAnExistingGroup is the half of #659 that is easy
// to miss and would otherwise strand every current user.
//
// The reuse path returned the existing group the moment it was found, without
// looking at its rules. Every `spawn-mpi-*` group already created carries only
// the old tcp rule, so upgrading spawn would fix nothing: the group is reused
// as-is, EFA stays broken, and the only workaround is deleting the group by
// hand — which fails while any instance still references it.
func TestMPISecurityGroupBackfillsAnExistingGroup(t *testing.T) {
	c, vpcID := mpiTestClient(t)
	ctx := context.Background()
	ec2c := c.regionalEC2("us-east-1")

	// Build a group the way spawn used to: tcp-only, self-referential.
	created, err := ec2c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("spawn-mpi-legacy-659"),
		Description: aws.String("legacy tcp-only MPI group"),
		VpcId:       aws.String(vpcID),
	})
	if err != nil {
		if strings.Contains(err.Error(), "InvalidAction") {
			t.Skipf("emulator serves no security-group API: %v", err)
		}
		t.Fatalf("CreateSecurityGroup: %v", err)
	}
	legacyID := *created.GroupId

	if _, err := ec2c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(legacyID),
		IpPermissions: []types.IpPermission{{
			IpProtocol:       aws.String("tcp"),
			FromPort:         aws.Int32(0),
			ToPort:           aws.Int32(65535),
			UserIdGroupPairs: []types.UserIdGroupPair{{GroupId: aws.String(legacyID)}},
		}},
	}); err != nil {
		t.Fatalf("seed legacy tcp rule: %v", err)
	}

	// Confirm the fixture really is the broken shape, so a pass cannot come from
	// the seeding having silently failed.
	if protos := selfReferentialProtocols(t, c, legacyID, false); containsProto(protos, "-1") {
		t.Fatalf("fixture already allows all traffic (%v); it cannot demonstrate a backfill", protos)
	}

	// Now ask spawn for that same group name.
	gotID, err := c.CreateOrGetMPISecurityGroup(ctx, "us-east-1", vpcID, "spawn-mpi-legacy-659")
	if err != nil {
		t.Fatalf("CreateOrGetMPISecurityGroup on an existing group: %v", err)
	}
	if gotID != legacyID {
		t.Fatalf("reuse returned %s, want the existing %s — a second group would orphan the "+
			"first and still not fix it", gotID, legacyID)
	}

	if protos := selfReferentialProtocols(t, c, legacyID, false); !containsProto(protos, "-1") {
		t.Errorf("after reuse, self-referential ingress = %v, want \"-1\" added.\n\n"+
			"Reusing a group without checking its rules means every group created before "+
			"this fix stays broken, and upgrading spawn changes nothing for existing "+
			"users (#659).", protos)
	}
}

// TestMPISecurityGroupIsIdempotent: the backfill runs on every launch, so a
// duplicate-rule error must not fail the launch.
func TestMPISecurityGroupIsIdempotent(t *testing.T) {
	c, vpcID := mpiTestClient(t)
	ctx := context.Background()

	first, err := c.CreateOrGetMPISecurityGroup(ctx, "us-east-1", vpcID, "spawn-mpi-idem-659")
	if err != nil {
		if strings.Contains(err.Error(), "InvalidAction") {
			t.Skipf("emulator serves no security-group API: %v", err)
		}
		t.Fatalf("first call: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := c.CreateOrGetMPISecurityGroup(ctx, "us-east-1", vpcID, "spawn-mpi-idem-659")
		if err != nil {
			t.Fatalf("call %d must tolerate already-present rules, got: %v", i+2, err)
		}
		if again != first {
			t.Fatalf("call %d returned %s, want the same group %s", i+2, again, first)
		}
	}
}

func containsProto(protos []string, want string) bool {
	for _, p := range protos {
		if p == want {
			return true
		}
	}
	return false
}
