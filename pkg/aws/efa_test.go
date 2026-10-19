package aws

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// capacityOf builds the capacity DescribeInstanceTypes reports for an n-card
// type whose every card takes interface, efa and efa-only — what c8gn.48xlarge
// (2), p5en.48xlarge (16) and p5.48xlarge (32) report in us-east-1.
func capacityOf(t *testing.T, instanceType string, n int) EFACapacity {
	t.Helper()
	ni := &types.NetworkInfo{
		EfaSupported: aws.Bool(true),
		EfaInfo:      &types.EfaInfo{MaximumEfaInterfaces: aws.Int32(int32(n))},
	}
	for i := 0; i < n; i++ {
		ni.NetworkCards = append(ni.NetworkCards, types.NetworkCardInfo{
			NetworkCardIndex: aws.Int32(int32(i)),
			InterfaceTypes: []types.NetworkCardInterfaceType{
				types.NetworkCardInterfaceTypeInterface, types.NetworkCardInterfaceTypeEfa, types.NetworkCardInterfaceTypeEfaOnly,
			},
		})
	}
	c, err := efaCapacityFromInfo(instanceType, types.InstanceTypeInfo{NetworkInfo: ni})
	if err != nil {
		t.Fatalf("efaCapacityFromInfo: %v", err)
	}
	return c
}

func layout(nis []types.InstanceNetworkInterfaceSpecification) string {
	var parts []string
	for _, ni := range nis {
		parts = append(parts, fmt.Sprintf("%d/%d/%s", aws.ToInt32(ni.NetworkCardIndex), aws.ToInt32(ni.DeviceIndex), aws.ToString(ni.InterfaceType)))
	}
	return strings.Join(parts, " ")
}

// The layout is the AWS EFA guide's for multi-card types: the primary at card
// 0 / device 0 (type efa, never efa-only), each further card at device 0 as
// efa-only, all in one subnet and security group set, deleted with the
// instance, and none asking for a public IP (EC2 refuses one with >1 interface).
func TestEFANetworkInterfaces_OnePerCard(t *testing.T) {
	for _, tc := range []struct {
		instanceType string
		cards        int
	}{{"c8gn.48xlarge", 2}, {"p5en.48xlarge", 16}, {"p5.48xlarge", 32}} {
		t.Run(tc.instanceType, func(t *testing.T) {
			c := capacityOf(t, tc.instanceType, tc.cards)
			n, err := c.ResolveInterfaces(0)
			if err != nil || n != tc.cards {
				t.Fatalf("ResolveInterfaces(0) = %d, %v; want every card (%d)", n, err, tc.cards)
			}
			nis, err := EFANetworkInterfaces("subnet-1", []string{"sg-1"}, c, n)
			if err != nil {
				t.Fatal(err)
			}
			if len(nis) != tc.cards {
				t.Fatalf("%d interfaces, want %d", len(nis), tc.cards)
			}
			want := []string{"0/0/efa"}
			for i := 1; i < tc.cards; i++ {
				want = append(want, fmt.Sprintf("%d/0/efa-only", i))
			}
			if got := layout(nis); got != strings.Join(want, " ") {
				t.Errorf("layout = %s\nwant     %s", got, strings.Join(want, " "))
			}
			for i, ni := range nis {
				if ni.NetworkCardIndex == nil {
					t.Errorf("interface %d: NetworkCardIndex unset", i)
				}
				if ni.AssociatePublicIpAddress != nil {
					t.Errorf("interface %d asks for a public IP; EC2 rejects that with %d interfaces", i, len(nis))
				}
				if aws.ToString(ni.SubnetId) != "subnet-1" || len(ni.Groups) != 1 || ni.Groups[0] != "sg-1" || !aws.ToBool(ni.DeleteOnTermination) {
					t.Errorf("interface %d = %+v, want subnet-1, sg-1, delete on termination", i, ni)
				}
			}
			if !NeedsLaunchElasticIP(nis) {
				t.Error("NeedsLaunchElasticIP = false for a multi-interface launch: it would have no public IP")
			}
		})
	}
}

// --efa-interfaces 1 is today's launch exactly: one efa interface with an
// auto-assigned public IP and no Elastic IP.
func TestEFANetworkInterfaces_OneIsTodaysInterface(t *testing.T) {
	c := capacityOf(t, "c8gn.48xlarge", 2)
	nis, err := EFALaunchInterfaces(LaunchConfig{SubnetID: "subnet-1", SecurityGroupIDs: []string{"sg-1"}, EFAEnabled: true, EFAInterfaces: 1}, c)
	if err != nil {
		t.Fatal(err)
	}
	single := EFANetworkInterface("subnet-1", []string{"sg-1"})
	if len(nis) != 1 || layout(nis) != layout([]types.InstanceNetworkInterfaceSpecification{single}) ||
		!aws.ToBool(nis[0].AssociatePublicIpAddress) || nis[0].NetworkCardIndex != nil {
		t.Fatalf("1 interface = %+v, want EFANetworkInterface's single interface", nis)
	}
	if NeedsLaunchElasticIP(nis) {
		t.Error("a single-interface launch must keep its auto-assigned public IP, not take an Elastic IP")
	}
	// An SDK caller that never sets EFAInterfaces gets the same.
	if zero, _ := EFALaunchInterfaces(LaunchConfig{EFAEnabled: true}, c); len(zero) != 1 {
		t.Errorf("EFAInterfaces 0 at the Launch layer = %d interfaces, want 1 (unchanged for SDK callers)", len(zero))
	}
}

func TestEFACapacity_ResolveInterfacesBounds(t *testing.T) {
	c := capacityOf(t, "p5en.48xlarge", 16)
	for _, tc := range []struct {
		req, want int
		wantErr   bool
	}{{0, 16, false}, {1, 1, false}, {16, 16, false}, {17, 0, true}, {-1, 0, true}} {
		got, err := c.ResolveInterfaces(tc.req)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ResolveInterfaces(%d) = %d, %v; want %d, err=%v", tc.req, got, err, tc.want, tc.wantErr)
		}
	}
	if _, err := EFANetworkInterfaces("", nil, c, 17); err == nil {
		t.Error("EFANetworkInterfaces(17) on a 16-card type: no error")
	}
}

// A type whose card 0 carries no EFA (P6-B300's primary card is ENA-only) gets
// an ENA primary and its EFAs on the cards that have them.
func TestEFANetworkInterfaces_Card0WithoutEFA(t *testing.T) {
	ni := &types.NetworkInfo{
		EfaSupported: aws.Bool(true),
		EfaInfo:      &types.EfaInfo{MaximumEfaInterfaces: aws.Int32(2)},
		NetworkCards: []types.NetworkCardInfo{
			{NetworkCardIndex: aws.Int32(0), InterfaceTypes: []types.NetworkCardInterfaceType{types.NetworkCardInterfaceTypeInterface}},
			{NetworkCardIndex: aws.Int32(1), InterfaceTypes: []types.NetworkCardInterfaceType{types.NetworkCardInterfaceTypeEfaOnly}},
			{NetworkCardIndex: aws.Int32(2), InterfaceTypes: []types.NetworkCardInterfaceType{types.NetworkCardInterfaceTypeEfaOnly}},
		},
	}
	c, err := efaCapacityFromInfo("p6-b300.48xlarge", types.InstanceTypeInfo{NetworkInfo: ni})
	if err != nil {
		t.Fatal(err)
	}
	nis, err := EFANetworkInterfaces("", nil, c, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := layout(nis); got != "0/0/ 1/0/efa-only 2/0/efa-only" {
		t.Errorf("layout = %s, want an ENA primary then efa-only on cards 1 and 2", got)
	}
}

func TestEFACapacity_NotSupported(t *testing.T) {
	if _, err := efaCapacityFromInfo("t3.micro", types.InstanceTypeInfo{NetworkInfo: &types.NetworkInfo{EfaSupported: aws.Bool(false)}}); err == nil ||
		!strings.Contains(err.Error(), "does not support EFA") {
		t.Errorf("err = %v, want does not support EFA", err)
	}
	// No per-card list (an older response): one card per MaximumEfaInterfaces.
	c, err := efaCapacityFromInfo("x", types.InstanceTypeInfo{NetworkInfo: &types.NetworkInfo{
		EfaSupported: aws.Bool(true), EfaInfo: &types.EfaInfo{MaximumEfaInterfaces: aws.Int32(4)},
	}})
	if err != nil || c.MaxInterfaces != 4 || len(c.Cards) != 4 {
		t.Errorf("capacity = %+v, %v; want 4 synthesized cards", c, err)
	}
}

// fakeEIPAPI is EC2 for the launch Elastic IP lifecycle.
type fakeEIPAPI struct {
	states      []types.InstanceStateName // successive DescribeInstances answers; last repeats
	describes   int
	addrs       []types.Address
	allocTags   []types.Tag
	allocated   []string
	assocErr    []error // successive AssociateAddress errors; nil after
	associated  map[string]string
	released    []string
	disassoc    []string
	releaseErrs []error
}

func (f *fakeEIPAPI) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	st := f.states[min(f.describes, len(f.states)-1)]
	f.describes++
	inst := types.Instance{
		InstanceId: aws.String(in.InstanceIds[0]),
		State:      &types.InstanceState{Name: st},
		NetworkInterfaces: []types.InstanceNetworkInterface{
			{NetworkInterfaceId: aws.String("eni-card1"), Attachment: &types.InstanceNetworkInterfaceAttachment{NetworkCardIndex: aws.Int32(1), DeviceIndex: aws.Int32(0)}},
			{NetworkInterfaceId: aws.String("eni-primary"), Attachment: &types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)}},
		},
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{inst}}}}, nil
}

func (f *fakeEIPAPI) DescribeAddresses(context.Context, *ec2.DescribeAddressesInput, ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	return &ec2.DescribeAddressesOutput{Addresses: f.addrs}, nil
}

func (f *fakeEIPAPI) AllocateAddress(_ context.Context, in *ec2.AllocateAddressInput, _ ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error) {
	if in.Domain != types.DomainTypeVpc || len(in.TagSpecifications) != 1 || in.TagSpecifications[0].ResourceType != types.ResourceTypeElasticIp {
		return nil, errors.New("bad allocate input")
	}
	f.allocTags = in.TagSpecifications[0].Tags
	id := fmt.Sprintf("eipalloc-%d", len(f.allocated)+1)
	f.allocated = append(f.allocated, id)
	return &ec2.AllocateAddressOutput{AllocationId: aws.String(id), PublicIp: aws.String("198.51.100.7")}, nil
}

func (f *fakeEIPAPI) AssociateAddress(_ context.Context, in *ec2.AssociateAddressInput, _ ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error) {
	if len(f.assocErr) > 0 {
		err := f.assocErr[0]
		f.assocErr = f.assocErr[1:]
		if err != nil {
			return nil, err
		}
	}
	if f.associated == nil {
		f.associated = map[string]string{}
	}
	f.associated[aws.ToString(in.AllocationId)] = aws.ToString(in.NetworkInterfaceId)
	return &ec2.AssociateAddressOutput{AssociationId: aws.String("eipassoc-1")}, nil
}

func (f *fakeEIPAPI) DisassociateAddress(_ context.Context, in *ec2.DisassociateAddressInput, _ ...func(*ec2.Options)) (*ec2.DisassociateAddressOutput, error) {
	f.disassoc = append(f.disassoc, aws.ToString(in.AssociationId))
	return &ec2.DisassociateAddressOutput{}, nil
}

func (f *fakeEIPAPI) ReleaseAddress(_ context.Context, in *ec2.ReleaseAddressInput, _ ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	if len(f.releaseErrs) > 0 {
		err := f.releaseErrs[0]
		f.releaseErrs = f.releaseErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	f.released = append(f.released, aws.ToString(in.AllocationId))
	return &ec2.ReleaseAddressOutput{}, nil
}

func fastEIPPolling(t *testing.T) {
	t.Helper()
	pi, rt, rb := eipPollInterval, eipRunningTimeout, eipRetryBackoff
	eipPollInterval, eipRunningTimeout, eipRetryBackoff = time.Millisecond, time.Second, []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { eipPollInterval, eipRunningTimeout, eipRetryBackoff = pi, rt, rb })
}

func tagMap(tags []types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// Allocate after running, tagged so terminate and the reaper can find it, and
// associated to the primary interface (card 0, device 0) — not to whichever
// interface is listed first.
func TestAttachLaunchElasticIP_AllocatesTagsAndAssociatesPrimary(t *testing.T) {
	fastEIPPolling(t)
	f := &fakeEIPAPI{
		states:   []types.InstanceStateName{types.InstanceStateNamePending, types.InstanceStateNameRunning},
		assocErr: []error{errors.New("InvalidNetworkInterfaceID.NotFound: not yet")}, // eventual consistency, retried
	}
	instTags := []types.Tag{{Key: aws.String("spawn:iam-user"), Value: aws.String("arn:aws:iam::1:user/u")}}
	ip, alloc, err := attachLaunchElasticIP(context.Background(), f, "proxy-1", "i-1", instTags)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "198.51.100.7" || alloc != "eipalloc-1" {
		t.Errorf("ip, alloc = %s, %s", ip, alloc)
	}
	if f.describes < 2 {
		t.Error("allocated before the instance was running")
	}
	if got := f.associated["eipalloc-1"]; got != "eni-primary" {
		t.Errorf("associated to %q, want the primary interface eni-primary", got)
	}
	tags := tagMap(f.allocTags)
	for k, want := range map[string]string{
		"spawn:managed": "true", ElasticIPInstanceTagKey: "i-1", "spawn:purpose": ElasticIPPurpose,
		"Name": "proxy-1", "spawn:iam-user": "arn:aws:iam::1:user/u",
	} {
		if tags[k] != want {
			t.Errorf("tag %s = %q, want %q", k, tags[k], want)
		}
	}
	if tags[CreatedTagKey] == "" {
		t.Errorf("no %s tag: the reaper could not age the address", CreatedTagKey)
	}
}

// A failed association releases the address it allocated, so the caller has
// only the instance to clean up.
func TestAttachLaunchElasticIP_FailureReleasesWhatItAllocated(t *testing.T) {
	fastEIPPolling(t)
	f := &fakeEIPAPI{
		states:   []types.InstanceStateName{types.InstanceStateNameRunning},
		assocErr: []error{errors.New("UnauthorizedOperation")},
	}
	if _, _, err := attachLaunchElasticIP(context.Background(), f, "", "i-1", nil); err == nil {
		t.Fatal("no error from a failed association")
	}
	if len(f.allocated) != 1 || len(f.released) != 1 || f.released[0] != f.allocated[0] {
		t.Errorf("allocated %v, released %v: the address would leak and bill", f.allocated, f.released)
	}
}

// An instance that ends before running is an error, and nothing is allocated.
func TestAttachLaunchElasticIP_InstanceEndedAllocatesNothing(t *testing.T) {
	fastEIPPolling(t)
	f := &fakeEIPAPI{states: []types.InstanceStateName{types.InstanceStateNamePending, types.InstanceStateNameTerminated}}
	if _, _, err := attachLaunchElasticIP(context.Background(), f, "", "i-1", nil); err == nil {
		t.Fatal("no error for an instance that terminated before running")
	}
	if len(f.allocated) != 0 {
		t.Errorf("allocated %v for an instance that never ran", f.allocated)
	}
}

// A retried launch (same ClientToken, same instance) reuses the address tagged
// for the instance instead of allocating a second.
func TestAttachLaunchElasticIP_ReusesExisting(t *testing.T) {
	fastEIPPolling(t)
	f := &fakeEIPAPI{
		states: []types.InstanceStateName{types.InstanceStateNameRunning},
		addrs:  []types.Address{{AllocationId: aws.String("eipalloc-old"), PublicIp: aws.String("203.0.113.9")}},
	}
	ip, alloc, err := attachLaunchElasticIP(context.Background(), f, "", "i-1", nil)
	if err != nil || alloc != "eipalloc-old" || ip != "203.0.113.9" {
		t.Fatalf("= %s, %s, %v; want the existing address", ip, alloc, err)
	}
	if len(f.allocated) != 0 || f.associated["eipalloc-old"] != "eni-primary" {
		t.Errorf("allocated %v, associated %v", f.allocated, f.associated)
	}
}

// Terminate's release: disassociate, then release, retrying the InUse race.
func TestReleaseInstanceElasticIPs(t *testing.T) {
	fastEIPPolling(t)
	f := &fakeEIPAPI{
		addrs:       []types.Address{{AllocationId: aws.String("eipalloc-1"), AssociationId: aws.String("eipassoc-1")}},
		releaseErrs: []error{errors.New("InvalidIPAddress.InUse")},
	}
	released, err := releaseInstanceElasticIPs(context.Background(), f, "i-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.disassoc) != 1 || f.disassoc[0] != "eipassoc-1" {
		t.Errorf("disassociated %v, want eipassoc-1", f.disassoc)
	}
	if len(released) != 1 || released[0] != "eipalloc-1" {
		t.Errorf("released %v", released)
	}

	// None tagged for the instance: nothing to do, no error.
	if r, err := releaseInstanceElasticIPs(context.Background(), &fakeEIPAPI{}, "i-2"); err != nil || len(r) != 0 {
		t.Errorf("= %v, %v", r, err)
	}
}

// The terminate path must call the release, and only after the terminate: a
// release placed before a terminate that then fails would strip a running
// instance of its address.
func TestTerminateReleasesElasticIPAfterTerminating(t *testing.T) {
	b, err := methodSource("client.go", "Terminate")
	if err != nil {
		t.Fatal(err)
	}
	term := strings.Index(b, "spotrequest.Terminate(")
	rel := strings.Index(b, "releaseInstanceElasticIPs(ctx, ec2Client, instanceID)")
	if term < 0 || rel < 0 || rel < term {
		t.Fatalf("Terminate must call releaseInstanceElasticIPs after spotrequest.Terminate (terminate at %d, release at %d)", term, rel)
	}
}

// methodSource returns the source of the *Client method name in file.
func methodSource(file, name string) (string, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		return "", err
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Recv != nil && fd.Name.Name == name {
			return string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset]), nil
		}
	}
	return "", fmt.Errorf("method %s not found in %s", name, file)
}

// Through Launch: --efa with 2 interfaces on a 2-card type sends one interface
// per card with no public IP request, then allocates and associates an Elastic
// IP to the primary interface and returns it as the launch's public IP.
func TestLaunch_MultiEFAAssociatesElasticIP(t *testing.T) {
	fastEIPPolling(t)
	f := &fakeEC2{efaCards: 2}
	c := newFakeEC2Client(t, f)
	res, err := c.Launch(context.Background(), LaunchConfig{
		InstanceType: "c8gn.48xlarge", Region: "us-west-2", AMI: "ami-0123456789abcdef0", PricePerHour: 1,
		EFAEnabled: true, EFAInterfaces: 2, SubnetID: "subnet-1", SecurityGroupIDs: []string{"sg-1"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	run := f.request("RunInstances")
	for k, want := range map[string]string{
		"NetworkInterface.1.NetworkCardIndex": "0", "NetworkInterface.1.DeviceIndex": "0", "NetworkInterface.1.InterfaceType": "efa",
		"NetworkInterface.2.NetworkCardIndex": "1", "NetworkInterface.2.DeviceIndex": "0", "NetworkInterface.2.InterfaceType": "efa-only",
		"NetworkInterface.1.AssociatePublicIpAddress": "", "NetworkInterface.2.AssociatePublicIpAddress": "",
	} {
		if got := run.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := f.request("AssociateAddress").Get("NetworkInterfaceId"); got != "eni-primary" {
		t.Errorf("associated to %q, want eni-primary", got)
	}
	if res.PublicIP != "198.51.100.9" {
		t.Errorf("PublicIP = %q, want the Elastic IP", res.PublicIP)
	}
}

// If the Elastic IP cannot be attached, the instance is terminated — not left
// running with no public address — and Launch fails saying so.
func TestLaunch_MultiEFAElasticIPFailureTerminates(t *testing.T) {
	fastEIPPolling(t)
	f := &fakeEC2{efaCards: 2, allocDenied: true}
	c := newFakeEC2Client(t, f)
	_, err := c.Launch(context.Background(), LaunchConfig{
		InstanceType: "c8gn.48xlarge", Region: "us-west-2", AMI: "ami-0123456789abcdef0", PricePerHour: 1,
		EFAEnabled: true, EFAInterfaces: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("err = %v, want a launch failure that says the instance was terminated", err)
	}
	if f.request("TerminateInstances") == nil {
		t.Errorf("no TerminateInstances (actions %v): the instance would run unreachable", f.actions())
	}
}
