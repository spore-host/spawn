package aws

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	return EFANetworkInterfacesWithSecondaryType(subnetID, securityGroupIDs, capacity, n, "")
}

// EFANetworkInterfacesWithSecondaryType is [EFANetworkInterfaces] with the
// interface type of the cards after card 0 chosen (#1404): "" or
// [EFASecondaryTypeEFAOnly] is the layout above; [EFASecondaryTypeEFA] makes
// the interface on every card with NetworkCardIndex >= 1 type "efa" (EFA with
// ENA) at device index 0, so each card also carries IP traffic on a private IP
// of its own. Card 0 is unchanged either way, and so is the absence of any
// public IP request: the Elastic IP still goes to the primary only.
//
// What this relies on, from the EC2 documentation (read 2026-10-28):
//
//   - "You can assign only one EFA or EFA-only network interface per network
//     card" (User Guide, Elastic network interfaces > Network cards,
//     https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/using-eni.html#network-cards),
//     so an "efa" interface replaces the card's "efa-only" one rather than
//     joining it; device index 0 is the slot the multi-card guide gives the
//     card's EFA.
//   - InterfaceType is "interface | efa | efa-only", and only efa-only "do[es]
//     not support IP addresses" (API Reference,
//     InstanceNetworkInterfaceSpecification); "EFA (EFA with ENA) interfaces
//     provide both the ENA device for IP networking and the EFA device"
//     (User Guide, Elastic Fabric Adapter > EFA basics).
//   - Whether a card accepts "efa" is per card: DescribeInstanceTypes
//     NetworkInfo.NetworkCards[].InterfaceTypes, read into
//     [EFANetworkCard.EFA]. A card that does not is refused here, before
//     RunInstances.
//   - Every interface keeps the primary's subnet and security groups. Nothing
//     in RunInstances requires a different subnet for a secondary interface;
//     the User Guide warns that several interfaces in one subnet "might
//     encounter networking issues such as asymmetric routing"
//     (network-interface-attachments.html), which is what the OS's policy
//     routing for secondary interfaces addresses (see the --efa-secondary-type
//     help). Each such interface takes one more private IP from the subnet.
//
// Not verified offline: that RunInstances accepts "efa" at device index 0 on
// card >= 1 for every multi-card type (the guide's examples use efa-only there,
// with ENA at device index 1); that is the owner's c8gn.48xlarge test (#1404).
func EFANetworkInterfacesWithSecondaryType(subnetID string, securityGroupIDs []string, capacity EFACapacity, n int, secondaryType string) ([]types.InstanceNetworkInterfaceSpecification, error) {
	if err := validEFASecondaryTypeValue(secondaryType); err != nil {
		return nil, err
	}
	secondaryEFA := secondaryType == EFASecondaryTypeEFA
	if n < 1 {
		n = 1
	}
	if n > capacity.MaxInterfaces || n > len(capacity.Cards) {
		return nil, fmt.Errorf("%d EFA interface(s) requested but %s supports %d", n, capacity.InstanceType, capacity.MaxInterfaces)
	}
	card0EFA := len(capacity.Cards) > 0 && capacity.Cards[0].Index == 0 && capacity.Cards[0].EFA
	if n == 1 && card0EFA {
		if secondaryEFA {
			return nil, fmt.Errorf("--efa-secondary-type %s needs more than one EFA interface (a network card after card 0), but this launch has 1", secondaryType)
		}
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
		case secondaryEFA:
			if !card.EFA {
				return nil, fmt.Errorf("--efa-secondary-type efa: network card %d of %s does not accept an efa (EFA with ENA) interface "+
					"(DescribeInstanceTypes lists only %s for it); use --efa-secondary-type efa-only", card.Index, capacity.InstanceType, cardTypes(card))
			}
			out = append(out, spec(card.Index, 0, EFASecondaryTypeEFA))
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
	if err := cfg.ValidateEFASecondaryType(); err != nil {
		return nil, err
	}
	if cfg.EFAInterfaces > 1 {
		return EFANetworkInterfacesWithSecondaryType(cfg.SubnetID, cfg.SecurityGroupIDs, capacity, cfg.EFAInterfaces, cfg.EFASecondaryType)
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
	switch {
	case aws.ToInt32(ni.NetworkCardIndex) == 0 && aws.ToInt32(ni.DeviceIndex) == 0:
		s += " (primary)"
	case t == EFASecondaryTypeEFAOnly:
		s += " (no IP)"
	default:
		s += " (private IP)"
	}
	return s
}

// The interface types --efa-secondary-type takes (#1404): what the interface
// on each network card after card 0 of a multi-card EFA launch is.
const (
	// EFASecondaryTypeEFAOnly is the default: an efa-only interface, EFA
	// device only, no IP address. All IP traffic uses card 0's interface.
	EFASecondaryTypeEFAOnly = "efa-only"
	// EFASecondaryTypeEFA is an efa (EFA with ENA) interface: the EFA device
	// plus an ENA device with a private IP, so the card carries IP traffic too.
	EFASecondaryTypeEFA = "efa"
)

func validEFASecondaryTypeValue(v string) error {
	switch v {
	case "", EFASecondaryTypeEFAOnly, EFASecondaryTypeEFA:
		return nil
	}
	return fmt.Errorf("--efa-secondary-type must be %s or %s, got %q", EFASecondaryTypeEFAOnly, EFASecondaryTypeEFA, v)
}

// ValidateEFASecondaryType refuses an EFASecondaryType that is not "",
// "efa-only" or "efa", and an "efa" without a multi-card EFA launch to apply
// it to: EFAEnabled with EFAInterfaces above 1 (at this layer 0 means one
// interface, as it always has). [Client.Launch] calls it before any AWS call;
// the CLI resolves --efa-interfaces 0 to the type's count first.
func (cfg LaunchConfig) ValidateEFASecondaryType() error {
	if err := validEFASecondaryTypeValue(cfg.EFASecondaryType); err != nil {
		return err
	}
	if cfg.EFASecondaryType != EFASecondaryTypeEFA {
		return nil
	}
	if !cfg.EFAEnabled {
		return fmt.Errorf("--efa-secondary-type %s requires --efa", cfg.EFASecondaryType)
	}
	if cfg.EFAInterfaces <= 1 {
		return fmt.Errorf("--efa-secondary-type %s applies to the network cards after card 0 of a multi-card EFA launch, "+
			"but this launch has one EFA interface (--efa-interfaces %d on %s). Use a type with more than one EFA "+
			"network card and --efa-interfaces 0 (all) or 2 or more", cfg.EFASecondaryType, cfg.EFAInterfaces, orUnknownType(cfg.InstanceType))
	}
	return nil
}

// ErrEFANeedsSubnet is wrapped by [LaunchConfig.ValidateEFASubnet]: a launch
// with more than one EFA interface named neither a subnet nor an availability
// zone to find one in (#1410).
var ErrEFANeedsSubnet = errors.New("EFA interfaces need one subnet")

// ValidateEFASubnet refuses a multi-card EFA launch (EFAEnabled, EFAInterfaces
// above 1) with neither SubnetID nor AvailabilityZone (#1410). RunInstances
// wants an explicit subnet on every interface once there is more than one, and
// [EFALaunchInterfaces] puts them all in the launch's one subnet, so without a
// subnet or an AZ to pick one in there is nothing to lay them out in. Pure, so
// a caller can run it before anything is created: [Client.Launch] reached the
// same refusal only after the key pair and the instance profile existed.
func (cfg LaunchConfig) ValidateEFASubnet() error {
	if !cfg.EFAEnabled || cfg.EFAInterfaces <= 1 || cfg.SubnetID != "" || cfg.AvailabilityZone != "" {
		return nil
	}
	return fmt.Errorf("%d %w: pass --subnet-id, or --az to let spawn pick one", cfg.EFAInterfaces, ErrEFANeedsSubnet)
}

func orUnknownType(t string) string {
	if t == "" {
		return "this instance type"
	}
	return t
}

// cardTypes lists what a card accepts, for an error message.
func cardTypes(card EFANetworkCard) string {
	var ts []string
	if card.EFA {
		ts = append(ts, "efa")
	}
	if card.EFAOnly {
		ts = append(ts, "efa-only")
	}
	if len(ts) == 0 {
		return "no EFA type"
	}
	return strings.Join(ts, ", ")
}
