package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	libtags "github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/tagtest"

	"github.com/spore-host/spawn/internal/sdkanswer"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// The egress proxy's security groups and instance carry
// spore-host:managed-by=spawn (#1652): checked by the tagtest Detector on a
// real EC2 client.
func TestSporeTags_EgressProxyCreate(t *testing.T) {
	cfg := sdkanswer.Config("us-west-2", func(service, op string, in any) (any, error) {
		switch v := in.(type) {
		case *ec2.DescribeSecurityGroupsInput:
			if len(v.GroupIds) == 0 {
				return &ec2.DescribeSecurityGroupsOutput{}, nil
			}
			return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []types.SecurityGroup{{GroupId: aws.String(v.GroupIds[0])}}}, nil
		case *ec2.CreateSecurityGroupInput:
			return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-" + aws.ToString(v.GroupName))}, nil
		case *ec2.RunInstancesInput:
			return &ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-proxy")}}}, nil
		}
		return nil, nil
	})
	d := tagtest.New(t, libtags.ToolSpawn)
	d.Install(&cfg)
	api := ec2.NewFromConfig(cfg)
	ctx := context.Background()

	g, err := EnsureGroups(ctx, api, testVPC, t0)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	base, err := spawnaws.BuildLaunchTags(LaunchConfig("us-west-2", "0.0.0-test"), "123456789012",
		"arn:aws:iam::123456789012:user/u", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, Deps{EC2: api, Now: func() time.Time { return t0 }}, Spec{Region: "us-west-2", VPCID: testVPC,
		PublicSubnets: []spawnaws.PublicSubnet{{SubnetID: "subnet-1", AvailabilityZone: "us-west-2a"}},
		Groups:        g, AMI: "ami-1", InstanceProfile: "spored-instance-profile", BaseTags: base}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	seen := map[string]int{}
	for _, c := range d.Calls() {
		seen[c.Operation]++
	}
	if seen["CreateSecurityGroup"] != 2 || seen["RunInstances"] != 1 {
		t.Errorf("the Detector saw %v, want two CreateSecurityGroup and one RunInstances", seen)
	}
}
