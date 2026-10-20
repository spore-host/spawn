package aws

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EFANetworkCard is one network card of an instance type and whether it can
// carry an Elastic Fabric Adapter.
type EFANetworkCard struct {
	// Index is the card's NetworkCardIndex.
	Index int32
	// EFA reports that the card accepts InterfaceType "efa" (ENA + EFA).
	EFA bool
	// EFAOnly reports that the card accepts InterfaceType "efa-only".
	EFAOnly bool
}

// EFACapacity is what an instance type offers for EFA, from DescribeInstanceTypes
// (NetworkInfo.EfaInfo and NetworkInfo.NetworkCards).
//
// Multi-network-card types expose one EFA per card: c8gn.48xlarge has 2,
// p5en.48xlarge 16, p5.48xlarge 32. Attaching one interface uses one card, so a
// launch that ignores this runs on a fraction of the instance's fabric (#661).
type EFACapacity struct {
	InstanceType string
	// MaxInterfaces is NetworkInfo.EfaInfo.MaximumEfaInterfaces.
	MaxInterfaces int
	// Cards lists the network cards that can carry an EFA, in card-index order.
	Cards []EFANetworkCard
}

// EFACapacityInRegion describes the instance type's EFA capacity in the launch
// region. It errors when the type does not exist there or does not support EFA,
// exactly as [Client.ValidateInstanceTypeForEFAInRegion] does.
func (c *Client) EFACapacityInRegion(ctx context.Context, instanceType, region string) (EFACapacity, error) {
	ec2Client := c.regionalEC2(region)

	output, err := ec2Client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)},
	})
	if err != nil {
		return EFACapacity{}, fmt.Errorf("describe instance type %s: %w", instanceType, err)
	}
	if len(output.InstanceTypes) == 0 {
		return EFACapacity{}, fmt.Errorf("instance type %s not found", instanceType)
	}
	return efaCapacityFromInfo(instanceType, output.InstanceTypes[0])
}

// efaCapacityFromInfo is the pure decision core of EFACapacityInRegion.
//
// A card whose InterfaceTypes list is empty is assumed EFA-capable: older API
// responses (and emulators) do not report per-card interface types, and before
// this field existed every card of an EFA type took an EFA. When no card list
// is reported at all, cards 0..MaxInterfaces-1 are synthesized on that basis.
func efaCapacityFromInfo(instanceType string, info types.InstanceTypeInfo) (EFACapacity, error) {
	ni := info.NetworkInfo
	if ni == nil || !aws.ToBool(ni.EfaSupported) {
		return EFACapacity{}, fmt.Errorf("instance type %s does not support EFA", instanceType)
	}
	capacity := EFACapacity{InstanceType: instanceType, MaxInterfaces: 1}
	if ni.EfaInfo != nil && aws.ToInt32(ni.EfaInfo.MaximumEfaInterfaces) > 0 {
		capacity.MaxInterfaces = int(aws.ToInt32(ni.EfaInfo.MaximumEfaInterfaces))
	}
	for _, nc := range ni.NetworkCards {
		card := EFANetworkCard{Index: aws.ToInt32(nc.NetworkCardIndex)}
		if len(nc.InterfaceTypes) == 0 {
			card.EFA, card.EFAOnly = true, true
		}
		for _, t := range nc.InterfaceTypes {
			switch t {
			case types.NetworkCardInterfaceTypeEfa:
				card.EFA = true
			case types.NetworkCardInterfaceTypeEfaOnly:
				card.EFAOnly = true
			}
		}
		if card.EFA || card.EFAOnly {
			capacity.Cards = append(capacity.Cards, card)
		}
	}
	if len(ni.NetworkCards) == 0 {
		for i := 0; i < capacity.MaxInterfaces; i++ {
			capacity.Cards = append(capacity.Cards, EFANetworkCard{Index: int32(i), EFA: true, EFAOnly: true})
		}
	}
	sort.Slice(capacity.Cards, func(i, j int) bool { return capacity.Cards[i].Index < capacity.Cards[j].Index })
	if len(capacity.Cards) < capacity.MaxInterfaces {
		// The per-card list is the stricter of the two: one EFA per card.
		capacity.MaxInterfaces = len(capacity.Cards)
	}
	if capacity.MaxInterfaces < 1 {
		return EFACapacity{}, fmt.Errorf("instance type %s reports EFA support but no network card that can carry one", instanceType)
	}
	return capacity, nil
}

// ResolveInterfaces turns a requested EFA interface count into the count to
// launch with: 0 means every EFA the type offers (MaxInterfaces); a request
// above MaxInterfaces, or below zero, is an error.
func (capacity EFACapacity) ResolveInterfaces(requested int) (int, error) {
	switch {
	case requested < 0:
		return 0, fmt.Errorf("--efa-interfaces must be 0 (all) or a positive count, got %d", requested)
	case requested == 0:
		return capacity.MaxInterfaces, nil
	case requested > capacity.MaxInterfaces:
		return 0, fmt.Errorf("--efa-interfaces %d exceeds what %s supports (%d EFA interface(s), one per network card)",
			requested, capacity.InstanceType, capacity.MaxInterfaces)
	default:
		return requested, nil
	}
}

// EFANetworkInterfaces builds the RunInstances network interfaces for n EFAs,
// one per network card. n <= 1 on a type whose card 0 carries EFA returns
// exactly [EFANetworkInterface] — today's single interface, with its public IP.
//
// Layout, per the AWS EFA guide "Maximize network bandwidth on Amazon EC2
// instances with multiple network cards"
// (https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-acc-inst-types.html,
// read 2026-10-19):
//
//   - The primary interface is network card 0, device index 0. "You can't use an
//     EFA-only network interface as the primary network interface." spawn makes
//     it InterfaceType "efa" (ENA + EFA) when card 0 supports EFA, so card 0's
//     EFA and the instance's IP traffic share one interface; the guide's
//     alternative (an ENA primary plus an efa-only interface at card 0, device
//     index 1) gives the same fabric with one more interface. On a type whose
//     card 0 cannot carry EFA (P6-B300), the primary is a plain ENA interface.
//   - "For each additional network interface, use the next unused network card
//     index, device index 0, for EFA-only network interface." A card that
//     supports "efa" but not "efa-only" gets an "efa" interface at device index
//     1, the guide's earlier layout.
//
// Every interface uses the same subnet and security groups and is deleted with
// the instance. With more than one interface EC2 refuses to auto-assign a
// public IP ("You can't auto-assign public IP addresses with this
// configuration. However, you can attach an Elastic IP address to the primary
// network interface ... after launch"), so none is requested; [Client.Launch]
// associates an Elastic IP instead. [NeedsLaunchElasticIP] reports that case.
func EFANetworkInterfaces(subnetID string, securityGroupIDs []string, capacity EFACapacity, n int) ([]types.InstanceNetworkInterfaceSpecification, error) {
	if n < 1 {
		n = 1
	}
	if n > capacity.MaxInterfaces || n > len(capacity.Cards) {
		return nil, fmt.Errorf("%d EFA interface(s) requested but %s supports %d", n, capacity.InstanceType, capacity.MaxInterfaces)
	}
	card0EFA := len(capacity.Cards) > 0 && capacity.Cards[0].Index == 0 && capacity.Cards[0].EFA
	if n == 1 && card0EFA {
		return []types.InstanceNetworkInterfaceSpecification{EFANetworkInterface(subnetID, securityGroupIDs)}, nil
	}

	spec := func(card, device int32, interfaceType string) types.InstanceNetworkInterfaceSpecification {
		ni := types.InstanceNetworkInterfaceSpecification{
			NetworkCardIndex:    aws.Int32(card),
			DeviceIndex:         aws.Int32(device),
			DeleteOnTermination: aws.Bool(true),
		}
		if interfaceType != "" {
			ni.InterfaceType = aws.String(interfaceType)
		}
		if subnetID != "" {
			ni.SubnetId = aws.String(subnetID)
		}
		if len(securityGroupIDs) > 0 {
			ni.Groups = securityGroupIDs
		}
		return ni
	}

	var out []types.InstanceNetworkInterfaceSpecification
	rest := capacity.Cards
	if card0EFA {
		out = append(out, spec(0, 0, "efa"))
		rest = capacity.Cards[1:]
		n--
	} else {
		out = append(out, spec(0, 0, "")) // ENA primary: card 0 carries no EFA
	}
	for _, card := range rest[:n] {
		switch {
		case card.Index == 0:
			// Card 0 with efa-only but no "efa": the guide's "EFA-only network
			// interface for network card index 0, device index 1".
			out = append(out, spec(0, 1, "efa-only"))
		case card.EFAOnly:
			out = append(out, spec(card.Index, 0, "efa-only"))
		default:
			out = append(out, spec(card.Index, 1, "efa"))
		}
	}
	return out, nil
}

// EFALaunchInterfaces is the network interfaces [Client.Launch] sends for an
// EFA launch: [EFANetworkInterfaces] for EFAInterfaces > 1, otherwise the single
// [EFANetworkInterface] it always sent. Shared with `spawn launch --dry-run`,
// so the preview is the launch's own layout rather than a copy of it.
func EFALaunchInterfaces(cfg LaunchConfig, capacity EFACapacity) ([]types.InstanceNetworkInterfaceSpecification, error) {
	if cfg.EFAInterfaces > 1 {
		return EFANetworkInterfaces(cfg.SubnetID, cfg.SecurityGroupIDs, capacity, cfg.EFAInterfaces)
	}
	return []types.InstanceNetworkInterfaceSpecification{EFANetworkInterface(cfg.SubnetID, cfg.SecurityGroupIDs)}, nil
}

// NeedsLaunchElasticIP reports whether a launch with these interfaces gets its
// public IP from an Elastic IP associated after launch rather than from
// AssociatePublicIpAddress: EC2 auto-assigns a public IP only to an instance
// with a single network interface.
func NeedsLaunchElasticIP(interfaces []types.InstanceNetworkInterfaceSpecification) bool {
	return len(interfaces) > 1
}

// DescribeEFAInterface is a one-line description of a launch interface, for
// previews: "card 0 / device 0: efa (primary)".
func DescribeEFAInterface(ni types.InstanceNetworkInterfaceSpecification) string {
	t := aws.ToString(ni.InterfaceType)
	if t == "" {
		t = "interface (ENA)"
	}
	s := fmt.Sprintf("card %d / device %d: %s", aws.ToInt32(ni.NetworkCardIndex), aws.ToInt32(ni.DeviceIndex), t)
	if aws.ToInt32(ni.NetworkCardIndex) == 0 && aws.ToInt32(ni.DeviceIndex) == 0 {
		s += " (primary)"
	}
	return s
}
