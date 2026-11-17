// Package footprintspawn is spawn's half of the shared footprint schema
// (github.com/spore-host/libs/footprint, spawn#1653, part of the
// leave-no-trace epic spore-host/spore-host#752): the resource types spawn
// reports, the name patterns its teardown may delete, and the rules that give
// every resource in `spawn footprint -o json` its ownership and class.
//
// It is pure (no AWS calls), so the rules are tested on their own and every
// caller (the footprint scan in pkg/aws, the deleter in pkg/teardown, and the
// idle teardown to come, spawn#1654) applies the same ones.
//
// # Ownership
//
//   - tagged: spore-host:managed-by=spawn.
//   - foreign: spore-host:managed-by names another tool, or the name is one of
//     another tool's (calque-*, lagotto-*, truffle-*). The name rule wins over
//     a spawn tag: a calque-named resource is calque's whatever it carries, so
//     spawn never deletes it.
//   - legacy-name: neither; only the name or the old spawn:managed tag matched.
//     Reported, never deleted.
//
// # Class
//
// In this order, the first that applies:
//
//   - permanent: the spore-host/spawn-instance-boundary policy, anything tagged
//     spore-host:permanent=true or spore-host:role=service, and everything in
//     spore.host's infra account (InfraAccountID), whose service shares
//     spawn's tags with workload litter (spore-host/spore-host#750);
//   - user-data: what holds the user's data or is the user's (a results
//     bucket that is not provably empty, any other bucket, AMIs, snapshots,
//     a workstation key pair, an Elastic IP spawn did not allocate);
//   - active: in use now (an instance pending through stopped, an instance
//     profile on a live instance, a security group on a network interface, a
//     pool queue with workers, an FSx filesystem a live instance references,
//     an attached policy, a stack mid-operation, an enabled schedule);
//   - residue: spawn's own machinery, removable once spawn is idle (a deployed
//     stack, the shared spored role and profile);
//   - litter: anything else.
//
// # Deletable
//
// footprint.Deletable with Patterns: tagged, managed-by=spawn, not service,
// not permanent, class litter/residue/active, and a name Patterns registers
// for its type. Patterns lists only the types pkg/teardown has a deleter for
// (a test holds the two equal), so a type spawn only reports can never be
// planned for deletion.
package footprintspawn

import (
	"strings"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/tags"
)

// Tool is the footprint's tool.
const Tool = tags.ToolSpawn

// InfraAccountID is spore.host's shared infra account (docs/infra-account.md;
// lambda/ttl-reaper's InfraAccountID, which a reaper test holds equal). Every
// resource there is classed permanent, and `spawn teardown` refuses to run
// there at all.
const InfraAccountID = "966362334030"

// The resource types spawn reports, as CloudFormation type names. EC2 AMIs
// and snapshots have none; they are given the obvious ones.
const (
	TypeInstance        = "AWS::EC2::Instance"
	TypeVolume          = "AWS::EC2::Volume"
	TypeSecurityGroup   = "AWS::EC2::SecurityGroup"
	TypeKeyPair         = "AWS::EC2::KeyPair"
	TypePlacementGroup  = "AWS::EC2::PlacementGroup"
	TypeEIP             = "AWS::EC2::EIP"
	TypeImage           = "AWS::EC2::Image"
	TypeSnapshot        = "AWS::EC2::Snapshot"
	TypeSpotRequest     = "AWS::EC2::SpotInstanceRequest"
	TypeRole            = "AWS::IAM::Role"
	TypeInstanceProfile = "AWS::IAM::InstanceProfile"
	TypeManagedPolicy   = "AWS::IAM::ManagedPolicy"
	TypeBucket          = "AWS::S3::Bucket"
	TypeQueue           = "AWS::SQS::Queue"
	TypeTopic           = "AWS::SNS::Topic"
	TypeStack           = "AWS::CloudFormation::Stack"
	TypeSchedule        = "AWS::Scheduler::Schedule"
	TypeFunction        = "AWS::Lambda::Function"
	TypeLogGroup        = "AWS::Logs::LogGroup"
	TypeTable           = "AWS::DynamoDB::Table"
	TypeEventBus        = "AWS::Events::EventBus"
	TypeRule            = "AWS::Events::Rule"
	TypeFileSystem      = "AWS::FSx::FileSystem"
)

// RegionGlobal is the Region of an account-wide resource (IAM).
const RegionGlobal = "global"

// Patterns are the names spawn's teardown may delete, per type. Each is a
// whole-name match. Only the types pkg/teardown can delete appear; a pattern
// for anything else would let a plan name a resource no deleter handles.
//
// Several are deliberately broad (an instance or volume id, any key pair or
// placement group name): for those the name says nothing the tag does not,
// and what keeps a resource the user owns is its class (a workstation key is
// user-data) and the deleter's own refusal, both re-checked at the delete.
var Patterns = []footprint.NamePattern{
	footprint.MustPattern(TypeInstance, `i-[0-9a-f]{8,17}`),
	footprint.MustPattern(TypeVolume, `vol-[0-9a-f]{8,17}`),
	footprint.MustPattern(TypeSecurityGroup, `spawn-.+`),
	footprint.MustPattern(TypePlacementGroup, `.+`),
	footprint.MustPattern(TypeKeyPair, `.+`),
	footprint.MustPattern(TypeEIP, `eipalloc-[0-9a-f]{8,17}`),
	// Per-task and pool roles, and the shared spored identity.
	footprint.MustPattern(TypeRole, `spawn-instance-.+|spored-instance-role`),
	footprint.MustPattern(TypeInstanceProfile, `spawn-instance-.+|spored-instance-profile`),
	// The spored managed policies (pkg/instancerole.IsSporedPolicyName). The
	// boundary is at the same path and never matches: footprint.IsBoundary
	// refuses it before any pattern is read.
	footprint.MustPattern(TypeManagedPolicy, `(?:spored-policy|spored-baseline-policy|spored-nested-baseline-policy)-v[1-9][0-9]*`),
	// The per-account, per-region staging and results buckets
	// (pkg/bucketsweep), and nothing else.
	footprint.MustPattern(TypeBucket, `spawn-(?:staging|results)-[0-9]{12}-[a-z0-9-]+`),
	footprint.MustPattern(TypeQueue, `spawn-pool-.+`),
}

// DeletableTypes are the types Patterns registers, sorted as listed.
func DeletableTypes() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range Patterns {
		if !seen[p.Type] {
			seen[p.Type] = true
			out = append(out, p.Type)
		}
	}
	return out
}

// foreignNamePrefixes are the other tools' name prefixes. A resource named
// with one is that tool's, whatever its tags.
var foreignNamePrefixes = []tags.Tool{tags.ToolCalque, tags.ToolLagotto, tags.ToolTruffle}

// ForeignByName reports the tool whose name a resource carries, when it is
// not spawn's: calque-*, lagotto-* or truffle-*, after a log group's
// /aws/lambda/ or /aws/imagebuilder/ prefix.
func ForeignByName(name string) (tags.Tool, bool) {
	l := strings.ToLower(name)
	l = strings.TrimPrefix(l, "/aws/lambda/")
	l = strings.TrimPrefix(l, "/aws/imagebuilder/")
	for _, t := range foreignNamePrefixes {
		if strings.HasPrefix(l, string(t)) {
			return t, true
		}
	}
	return "", false
}

// OwnershipOf is a resource's ownership from its name and tags.
func OwnershipOf(name string, t map[string]string) footprint.Ownership {
	if _, ok := ForeignByName(name); ok {
		return footprint.OwnershipForeign
	}
	switch mb, ok := t[tags.ManagedBy]; {
	case ok && mb == string(Tool):
		return footprint.OwnershipTagged
	case ok && mb != "":
		return footprint.OwnershipForeign
	}
	return footprint.OwnershipLegacyName
}

// Facts are what the scan found out about a resource beyond its tags, for
// Classify. Each non-empty string is the reason that class applies.
type Facts struct {
	// Account is the account the resource is in.
	Account string
	// UserData: it holds, or is, the user's (why).
	UserData string
	// InUse: it is in use now (why).
	InUse string
	// Residue: it is spawn's own machinery (what).
	Residue string
}

// Classify is a resource's class, and why, by the rules in the package
// documentation.
func Classify(r footprint.Resource, f Facts) (footprint.Class, string) {
	switch {
	case footprint.IsBoundary(r):
		return footprint.ClassPermanent, "the account's spawn-instance permissions boundary, which every teardown keeps"
	case tags.IsService(r.Tags):
		return footprint.ClassPermanent, "spore.host's own service (" + tags.Role + "=" + tags.RoleService + ")"
	case tags.IsPermanent(r.Tags):
		return footprint.ClassPermanent, "tagged " + tags.Permanent + "=" + tags.PermanentTrue
	case f.Account == InfraAccountID:
		return footprint.ClassPermanent, "in spore.host's infra account, whose service shares spawn's tags"
	case f.UserData != "":
		return footprint.ClassUserData, f.UserData
	case f.InUse != "":
		return footprint.ClassActive, f.InUse
	case f.Residue != "":
		return footprint.ClassResidue, f.Residue
	}
	return footprint.ClassLitter, ""
}

// Finish fills r's Ownership, Class, Deletable and Reason from its name, tags
// and f. Reason is footprint.Deletable's (empty when deletable), followed by
// why the class applies when there is a why.
func Finish(r *footprint.Resource, f Facts) {
	r.Ownership = OwnershipOf(r.Name, r.Tags)
	var why string
	r.Class, why = Classify(*r, f)
	r.Deletable, r.Reason = footprint.Deletable(*r, Tool, Patterns)
	switch {
	case why == "":
	case r.Reason == "":
		r.Reason = string(r.Class) + ": " + why
	default:
		r.Reason += " (" + string(r.Class) + ": " + why + ")"
	}
}
