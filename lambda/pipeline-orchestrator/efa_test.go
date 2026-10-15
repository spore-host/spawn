package main

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spore-host/spawn/pkg/pipeline"
)

// ---- #956: efa_enabled attaches an EFA interface ----------------------------

// efaPipeline: "train" is a 2-instance EFA stage in us-west-2, "eval" an
// ordinary stage there. No streaming.
func efaPipeline() (*pipeline.PipelineState, *pipeline.Pipeline) {
	def := &pipeline.Pipeline{
		PipelineID: "pl-efa", PipelineName: "efa", S3Bucket: "bkt", S3Prefix: "pfx",
		Stages: []pipeline.Stage{
			{StageID: "train", InstanceType: "c5n.18xlarge", InstanceCount: 2, Region: "us-west-2", Command: "true", EFAEnabled: true},
			{StageID: "eval", InstanceType: "m5.large", InstanceCount: 1, Region: "us-west-2", Command: "true", DependsOn: []string{"train"}},
			{StageID: "report", InstanceType: "t3.micro", InstanceCount: 1, Region: "eu-west-1", Command: "true", DependsOn: []string{"eval"}},
		},
	}
	return &pipeline.PipelineState{PipelineID: "pl-efa", PipelineName: "efa", S3Bucket: "bkt", S3Prefix: "pfx"}, def
}

// An efa_enabled stage must be launched with an EFA network interface. It got
// only the placement group, so its instances had an ENA interface and
// `fi_info -p efa` found nothing. The request must also be one RunInstances
// accepts: groups on the interface, none at instance level.
func TestEFAStageIsLaunchedWithAnEFAInterface(t *testing.T) {
	installFakeNet(t, "us-east-1")
	state, def := efaPipeline()
	if err := setupNetworkResources(context.Background(), state, def); err != nil {
		t.Fatal(err)
	}
	sg := state.SecurityGroups["us-west-2"]
	if sg == "" {
		t.Fatalf("no security group in the EFA stage's region: %v", state.SecurityGroups)
	}

	in := buildStageRunInstancesInput(state, def.GetStage("train"), &pipeline.StageState{StageID: "train"}, 1, "ami-x", "ud", time.Now())
	if len(in.NetworkInterfaces) != 1 {
		t.Fatalf("EFA stage launched with %d network interfaces, want 1", len(in.NetworkInterfaces))
	}
	ni := in.NetworkInterfaces[0]
	if aws.ToString(ni.InterfaceType) != "efa" || aws.ToInt32(ni.DeviceIndex) != 0 || !aws.ToBool(ni.DeleteOnTermination) {
		t.Errorf("network interface = type %q, device %d, delete-on-termination %v; want an EFA primary interface deleted with the instance",
			aws.ToString(ni.InterfaceType), aws.ToInt32(ni.DeviceIndex), aws.ToBool(ni.DeleteOnTermination))
	}
	if len(ni.Groups) != 1 || ni.Groups[0] != sg {
		t.Errorf("EFA interface groups = %v, want [%s]", ni.Groups, sg)
	}
	if len(in.SecurityGroupIds) != 0 {
		t.Errorf("instance-level SecurityGroupIds %v alongside NetworkInterfaces; RunInstances rejects that", in.SecurityGroupIds)
	}
	if in.Placement == nil || aws.ToString(in.Placement.GroupName) != state.PlacementGroups["us-west-2"] {
		t.Errorf("EFA stage placement = %+v, want the cluster placement group", in.Placement)
	}

	// An ordinary stage keeps the default interface.
	plain := buildStageRunInstancesInput(state, def.GetStage("eval"), &pipeline.StageState{StageID: "eval"}, 0, "ami-x", "ud", time.Now())
	if len(plain.NetworkInterfaces) != 0 {
		t.Errorf("a stage without efa_enabled was given network interfaces %+v", plain.NetworkInterfaces)
	}
}

// EFA requires a group that admits all traffic from itself. The pipeline's
// group in a region with an EFA stage carries that rule (from itself only),
// even when nothing streams; a region with no EFA stage and no streaming gets
// no group at all.
func TestEFARegionGroupAdmitsAllTrafficFromItself(t *testing.T) {
	f := installFakeNet(t, "us-east-1")
	state, def := efaPipeline()
	if err := setupNetworkResources(context.Background(), state, def); err != nil {
		t.Fatal(err)
	}
	sg := state.SecurityGroups["us-west-2"]
	all := false
	for _, p := range f.ingress[sg] {
		if len(p.IpRanges) > 0 || len(p.Ipv6Ranges) > 0 || len(p.PrefixListIds) > 0 {
			t.Errorf("ingress open to an address range: %+v", p)
		}
		if aws.ToString(p.IpProtocol) == "-1" && len(p.UserIdGroupPairs) == 1 && aws.ToString(p.UserIdGroupPairs[0].GroupId) == sg {
			all = true
		}
	}
	if !all {
		t.Errorf("the EFA region's group %s does not admit all traffic from itself: %+v", sg, f.ingress[sg])
	}
	if _, ok := state.SecurityGroups["eu-west-1"]; ok {
		t.Error("a group was created in eu-west-1, which has no EFA stage and no streaming")
	}
}

// A streaming pipeline's group in a region with no EFA stage does not admit all
// traffic: only the streaming ports and SSH, as before.
func TestNonEFAGroupDoesNotAdmitAllTraffic(t *testing.T) {
	for _, p := range streamingIngress("sg-1", false) {
		if aws.ToString(p.IpProtocol) == "-1" {
			t.Errorf("non-EFA group admits all traffic: %+v", p)
		}
	}
	found := false
	for _, p := range streamingIngress("sg-1", true) {
		if aws.ToString(p.IpProtocol) == "-1" {
			found = true
		}
	}
	if !found {
		t.Error("EFA group has no all-traffic rule")
	}
}
