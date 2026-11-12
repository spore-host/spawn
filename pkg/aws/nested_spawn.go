package aws

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/launchconfig"
)

// Nested spawn (#39): `spawn launch --nested-spawn` gives the instance an IAM
// role that lets a spawn CLI running ON it launch and manage child instances,
// the way nf-spawn's head node dispatches tasks.
//
// It is a separate, opt-in role, never an addition to spored-instance-role.
// RunInstances plus iam:PassRole on every instance would let any spore start
// more spores with whatever role it chose, so the default role stays exactly as
// it was and only instances launched with the flag carry these grants.
//
// The role is named NestedSpawnRolePrefix + a hash of the grant document, so a
// CLI version with a different policy gets a different role rather than
// rewriting one a running head node uses. The prefix keeps it inside the
// spawn-instance-* family on purpose: pkg/instancerole (the TTL reaper's
// REAPER_INSTANCE_ROLES sweep and `spawn cleanup --instance-roles`) is its
// deleter, under the same rules as every per-task role — tagged
// spawn:managed=true, no live instance using the profile, 24h old and unused for
// 24h. No reaper or template change is needed for that, and none was made.
//
// What a nested spore can NOT do, by design:
//
//   - create or edit IAM. A spore that could CreateRole/PutRolePolicy could
//     write itself any policy and pass it to a child, which is the whole
//     account. So children use the shared spored-instance-profile (the parent
//     launch makes sure it exists), an existing profile named with
//     --instance-profile, or, with --nested-spawn, the parent's own nested
//     profile; any --iam-*/--s3-* flag is refused with a message (cmd
//     nestedChildProfile).
//   - pass any role outside spored-instance-role and spawn-instance-*.
//   - mutate anything not tagged spawn:managed=true, or create anything that is
//     not tagged spawn:managed=true at creation. The reaper scans exactly that
//     tag, so every child is in its scope from the first second.
//
// The calls this grants were derived from the CLI's own SDK calls for launch,
// list, status, extend, connect and terminate, and a test rediscovers them from
// source (cmd TestNestedSpawnPolicyCoversTheChildLaunchCalls), so a new call in
// those commands fails CI until it is granted here or listed as not granted with
// a reason.
const NestedSpawnRolePrefix = "spawn-instance-nested-"

// spawnManagedTag is the tag every spawn-created resource carries and the
// reaper scans.
const spawnManagedTag = "spawn:managed"

// nestedSpawnStatements is the grant document's statements. Ordering is
// stable so the role-name hash is deterministic.
func nestedSpawnStatements() []interface{} {
	acct := sporedAccountPlaceholder
	managedResource := map[string]interface{}{
		"StringEquals": map[string]interface{}{"ec2:ResourceTag/" + spawnManagedTag: "true"},
	}
	managedRequest := map[string]interface{}{
		"StringEquals": map[string]interface{}{"aws:RequestTag/" + spawnManagedTag: "true"},
	}
	ownChild := map[string]interface{}{
		"StringEquals": map[string]interface{}{"ec2:ResourceTag/" + NestedParentTagKey: "${aws:userid}"},
	}
	return []interface{}{
		// Reads. EC2 Describe* and these SSM/EFS reads have no resource-level
		// permissions, so "*" is the only form AWS accepts.
		map[string]interface{}{
			"Sid":    "NestedSpawnRead",
			"Effect": "Allow",
			"Action": []interface{}{
				"ec2:DescribeAddresses",
				"ec2:DescribeAvailabilityZones",
				"ec2:DescribeImages",
				"ec2:DescribeInstanceTypeOfferings",
				"ec2:DescribeInstanceTypes",
				"ec2:DescribeInstances",
				"ec2:DescribeKeyPairs",
				"ec2:DescribePlacementGroups",
				"ec2:DescribeRegions",
				"ec2:DescribeRouteTables",
				"ec2:DescribeSecurityGroups",
				"ec2:DescribeSnapshots",
				"ec2:DescribeSpotPriceHistory",
				"ec2:DescribeSubnets",
				"ec2:DescribeVpcEndpoints",
				"ec2:DescribeVpcs",
				"elasticfilesystem:DescribeFileSystems",
				"elasticfilesystem:DescribeMountTargets",
				"ssm:DescribeInstanceInformation",
				"ssm:GetCommandInvocation",
			},
			"Resource": "*",
		},
		// AMI auto-detection reads AWS's public parameters only (#38).
		map[string]interface{}{
			"Sid":      "NestedSpawnPublicAMIParameters",
			"Effect":   "Allow",
			"Action":   "ssm:GetParameter",
			"Resource": "arn:aws:ssm:*::parameter/aws/service/*",
		},
		// RunInstances, the resources it CREATES: the instance and its volumes
		// must be tagged spawn:managed=true in the request, so no child is ever
		// outside the reaper's scan. The instance must also carry
		// NestedParentTagKey set to this head's own ${aws:userid}: that tag is
		// what every grant below on an existing child is conditioned on, and
		// requiring the caller's own value here is what stops a head claiming
		// another instance's children.
		map[string]interface{}{
			"Sid":      "NestedSpawnRunTaggedInstances",
			"Effect":   "Allow",
			"Action":   "ec2:RunInstances",
			"Resource": "arn:aws:ec2:*:" + acct + ":instance/*",
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{
					"aws:RequestTag/" + spawnManagedTag:    "true",
					"aws:RequestTag/" + NestedParentTagKey: "${aws:userid}",
				},
			},
		},
		map[string]interface{}{
			"Sid":       "NestedSpawnRunTaggedVolumes",
			"Effect":    "Allow",
			"Action":    "ec2:RunInstances",
			"Resource":  "arn:aws:ec2:*:" + acct + ":volume/*",
			"Condition": managedRequest,
		},
		// RunInstances, the resources it only REFERENCES. AWS authorizes
		// RunInstances against every resource type it touches, and these are
		// pre-existing (an AMI, a subnet, the default security group) or created
		// untagged by EC2 itself (the primary network interface, a one-time Spot
		// request), so a tag condition would refuse every launch. Scoped by
		// resource type rather than "*"; the action is still RunInstances only,
		// which cannot succeed without the tagged instance above.
		map[string]interface{}{
			"Sid":    "NestedSpawnRunReferencedResources",
			"Effect": "Allow",
			"Action": "ec2:RunInstances",
			"Resource": []interface{}{
				"arn:aws:ec2:*::image/*",
				"arn:aws:ec2:*::snapshot/*",
				"arn:aws:ec2:*:*:subnet/*",
				"arn:aws:ec2:*:*:capacity-reservation/*",
				"arn:aws:ec2:*:" + acct + ":network-interface/*",
				"arn:aws:ec2:*:" + acct + ":security-group/*",
				"arn:aws:ec2:*:" + acct + ":key-pair/*",
				"arn:aws:ec2:*:" + acct + ":placement-group/*",
				"arn:aws:ec2:*:" + acct + ":spot-instances-request/*",
			},
		},
		// The other creates a launch makes: the SSH key, the MPI/Windows security
		// group, the MPI placement group. Each must be tagged spawn:managed=true
		// at creation (they all are: ImportKeyPair's managedResourceTags,
		// LifecycleTags), which is what `spawn cleanup` and the reaper find them by.
		map[string]interface{}{
			"Sid":    "NestedSpawnCreateTaggedResources",
			"Effect": "Allow",
			"Action": []interface{}{
				"ec2:ImportKeyPair",
				"ec2:CreateSecurityGroup",
				"ec2:CreatePlacementGroup",
			},
			"Resource": []interface{}{
				"arn:aws:ec2:*:" + acct + ":key-pair/*",
				"arn:aws:ec2:*:" + acct + ":security-group/*",
				"arn:aws:ec2:*:" + acct + ":placement-group/*",
			},
			"Condition": managedRequest,
		},
		// CreateSecurityGroup also authorizes against the VPC it is created in,
		// which carries no spawn tag.
		map[string]interface{}{
			"Sid":      "NestedSpawnCreateSecurityGroupInVPC",
			"Effect":   "Allow",
			"Action":   "ec2:CreateSecurityGroup",
			"Resource": "arn:aws:ec2:*:" + acct + ":vpc/*",
		},
		// Tag-on-create for exactly those creates. ec2:CreateAction is only
		// present when the tags are part of a create request, so this grants no
		// CreateTags on an existing resource (the baseline's spawn:managed-scoped
		// CreateTags covers those, e.g. `spawn extend`).
		map[string]interface{}{
			"Sid":      "NestedSpawnTagOnCreate",
			"Effect":   "Allow",
			"Action":   "ec2:CreateTags",
			"Resource": "*",
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{
					"ec2:CreateAction": []interface{}{
						"RunInstances",
						"ImportKeyPair",
						"CreateSecurityGroup",
						"CreatePlacementGroup",
					},
					"aws:RequestTag/" + spawnManagedTag: "true",
				},
			},
		},
		// Lifecycle of this head's own children only: start, stop, terminate,
		// read the Windows password, cancel the Spot request, and retag (`spawn
		// extend`). Every child carries NestedParentTagKey = this head's
		// ${aws:userid} from RunInstances (above), and no instance may write
		// that key afterwards (reaperControlTagKeys), so the condition reaches
		// exactly the instances this head launched. A persistent Spot request
		// is tagged with the instance's tags at the same RunInstances. The
		// condition key is EC2's (ec2:ResourceTag), because every action here
		// is EC2's.
		map[string]interface{}{
			"Sid":    "NestedSpawnManageOwnChildren",
			"Effect": "Allow",
			"Action": []interface{}{
				"ec2:StartInstances",
				"ec2:StopInstances",
				"ec2:TerminateInstances",
				"ec2:CancelSpotInstanceRequests",
				"ec2:GetPasswordData",
				"ec2:CreateTags",
				"ec2:DeleteTags",
			},
			"Resource":  "*",
			"Condition": ownChild,
		},
		// Security and placement groups spawn manages. The Revoke pair and
		// DeleteSecurityGroup are the egress proxy's (#1515): find-or-create
		// removes the default allow-all egress EC2 gives its two new groups
		// and any drifted rule (pkg/egressproxy/deploy EnsureGroups), and
		// `spawn egress-proxy delete` deletes the groups. Both groups carry
		// spawn:managed=true from creation, so the grant reaches them and no
		// group spawn did not tag. Groups are found-or-created by name and
		// shared with workstation launches (pkg/aws securitygroup.go), so they
		// carry no parent tag and stay scoped to spawn:managed=true.
		map[string]interface{}{
			"Sid":    "NestedSpawnManageManagedGroups",
			"Effect": "Allow",
			"Action": []interface{}{
				"ec2:AuthorizeSecurityGroupIngress",
				"ec2:AuthorizeSecurityGroupEgress",
				"ec2:RevokeSecurityGroupIngress",
				"ec2:RevokeSecurityGroupEgress",
				"ec2:DeleteSecurityGroup",
				"ec2:DeletePlacementGroup",
			},
			"Resource":  "*",
			"Condition": managedResource,
		},
		// `spawn connect` (Session Manager, through the aws CLI) and the
		// terminate-time task flush (SendCommand) — to this head's own
		// children only. The condition key is SSM's, because the action is
		// SSM's; ${aws:userid} is a global key, present on every request.
		map[string]interface{}{
			"Sid":      "NestedSpawnSSMToOwnChildren",
			"Effect":   "Allow",
			"Action":   []interface{}{"ssm:SendCommand", "ssm:StartSession"},
			"Resource": "arn:aws:ec2:*:" + acct + ":instance/*",
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{"ssm:resourceTag/" + NestedParentTagKey: "${aws:userid}"},
			},
		},
		map[string]interface{}{
			"Sid":    "NestedSpawnRunCommandDocuments",
			"Effect": "Allow",
			"Action": "ssm:SendCommand",
			"Resource": []interface{}{
				"arn:aws:ssm:*::document/AWS-RunShellScript",
				"arn:aws:ssm:*::document/AWS-RunPowerShellScript",
			},
		},
		map[string]interface{}{
			"Sid":      "NestedSpawnSessionDocument",
			"Effect":   "Allow",
			"Action":   "ssm:StartSession",
			"Resource": "arn:aws:ssm:*:*:document/SSM-SessionManagerRunShell",
		},
		// The launch config object of a child that has one (#1359): written to
		// the account's staging buckets under launch-config/ only, so a nested
		// spore can neither read another task's staged bootstrap nor write
		// anything else there. The bucket itself is created and configured by
		// a workstation launch; a nested spore does not get those grants.
		map[string]interface{}{
			"Sid":      "NestedSpawnStoreLaunchConfig",
			"Effect":   "Allow",
			"Action":   "s3:PutObject",
			"Resource": launchconfig.ObjectARNPattern(acct),
		},
		// PassRole to EC2 only, and only the two roles a nested child ever
		// gets (cmd nestedChildProfile): the shared default identity, and
		// this role itself, so a child may itself be nested. Not the
		// spawn-instance-* family: those roles carry whatever --iam-* flags
		// and task policies gave them, so passing them would hand a head node
		// the grants of every per-task role in the account. This role's name
		// is a hash of this document, so the document names it through
		// nestedSelfRolePlaceholder (see NestedSpawnRoleName).
		map[string]interface{}{
			"Sid":    "NestedSpawnPassSpawnRoles",
			"Effect": "Allow",
			"Action": "iam:PassRole",
			"Resource": []interface{}{
				"arn:aws:iam::" + acct + ":role/" + SporedRoleName,
				"arn:aws:iam::" + acct + ":role/" + nestedSelfRolePlaceholder,
			},
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{"iam:PassedToService": "ec2.amazonaws.com"},
			},
		},
	}
}

// nestedSelfRolePlaceholder stands for the nested role's own name in the
// grant document. The name is a hash of the document, so the document cannot
// carry it literally: NestedSpawnRoleName hashes the document with this
// placeholder in it, and nestedSpawnPolicyDoc then replaces the placeholder
// with that name. Not a valid role name, so an unbound document matches no
// role.
const nestedSelfRolePlaceholder = "SPAWN-NESTED-ROLE-SELF"

// nestedSpawnPolicyTemplate is the grant document with both placeholders.
func nestedSpawnPolicyTemplate() string {
	b, err := json.Marshal(map[string]interface{}{
		"Version":   "2012-10-17",
		"Statement": nestedSpawnStatements(),
	})
	if err != nil {
		panic(err) // static content; cannot fail
	}
	return string(b)
}

// nestedSpawnPolicyDoc is the grant document with the role's own name bound,
// still carrying sporedAccountPlaceholder (bound by sporedPolicyDoc before it
// is written).
func nestedSpawnPolicyDoc() string {
	return strings.ReplaceAll(nestedSpawnPolicyTemplate(), nestedSelfRolePlaceholder, NestedSpawnRoleName())
}

// NestedSpawnRoleName is the role and instance-profile name --nested-spawn
// creates or reuses: NestedSpawnRolePrefix plus 8 hex digits of the grant
// document's hash (taken with nestedSelfRolePlaceholder in place of the name
// itself). No AWS call; usable by --dry-run.
func NestedSpawnRoleName() string {
	sum := sha256.Sum256([]byte(nestedSpawnPolicyTemplate()))
	return fmt.Sprintf("%s%x", NestedSpawnRolePrefix, sum[:4])
}

// IsNestedSpawnRole reports whether roleName is a --nested-spawn role.
func IsNestedSpawnRole(roleName string) bool {
	return strings.HasPrefix(roleName, NestedSpawnRolePrefix)
}

// NestedSpawnRoleFromCallerARN returns the --nested-spawn role a caller is
// running as, from its sts:GetCallerIdentity ARN
// (arn:aws:sts::<acct>:assumed-role/<role>/<session>), and whether it is one.
// This is how the CLI on a nested spore knows it must not write IAM.
func NestedSpawnRoleFromCallerARN(arn string) (string, bool) {
	_, rest, ok := strings.Cut(arn, ":assumed-role/")
	if !ok {
		return "", false
	}
	role, _, _ := strings.Cut(rest, "/")
	if !IsNestedSpawnRole(role) {
		return "", false
	}
	return role, true
}

// nestedSpawnBaselinePolicyDoc is the spored baseline with one change: the
// tag Denies do not apply to tag-on-create.
//
// The reaper-control-tag Deny stops an instance writing spawn:managed,
// spawn:launch-time, spawn:pre-stop and the rest onto an EXISTING resource
// (GHSA-8q3p-qcv8-2pr4, GHSA-c93q-4jh7-6pvj), and DenyTagOthers stops it
// tagging anything but itself and its own children. IAM evaluates the tags of
// a RunInstances request as ec2:CreateTags too, so unchanged they would refuse
// every child launch: a child must be created carrying exactly those keys, and
// is not yet anyone's child. ec2:CreateAction is present only in that
// create-time evaluation, so requiring it to be absent keeps both Denies whole
// for every existing resource. Only the nested role gets this variant; the
// default baseline is unchanged. The stop/terminate Deny is left as it is: it
// never sees a create.
func (c *Client) nestedSpawnBaselinePolicyDoc() string {
	statements := c.sporedSelfManagementStatements()
	for i, st := range statements {
		m, ok := st.(map[string]interface{})
		if !ok || m["Effect"] != "Deny" || !policyActionIncludes(m["Action"], "ec2:CreateTags") {
			continue
		}
		cp := make(map[string]interface{}, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cond := map[string]interface{}{}
		if old, ok := m["Condition"].(map[string]interface{}); ok {
			for k, v := range old {
				cond[k] = v
			}
		}
		cond["Null"] = map[string]interface{}{"ec2:CreateAction": "true"}
		cp["Condition"] = cond
		statements[i] = cp
	}
	b, err := json.Marshal(map[string]interface{}{
		"Version":   "2012-10-17",
		"Statement": statements,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// SetupNestedSpawnProfile creates or reuses the --nested-spawn role and
// instance profile, and returns the profile name.
//
// It first makes sure the shared spored-instance-profile exists
// (SetupSporedIAMRole, unchanged): a nested spore cannot create IAM, so the
// default identity its children use has to exist before it boots.
func (c *Client) SetupNestedSpawnProfile(ctx context.Context) (string, error) {
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		return "", fmt.Errorf("set up %s for the nested spore's children: %w", SporedInstanceProfileName, err)
	}
	doc, err := c.sporedPolicyDoc(ctx, nestedSpawnPolicyDoc())
	if err != nil {
		return "", err
	}
	return c.createOrGetInstanceProfile(ctx, IAMRoleConfig{
		RoleName:         NestedSpawnRoleName(),
		InlinePolicyJSON: doc,
		Tags:             map[string]string{"spawn:purpose": "nested-spawn"},
	}, c.nestedSpawnBaselinePolicyDoc(), sporedNestedBaselinePolicyBase)
}

// policyActionIncludes reports whether a statement's Action (a string or a
// list of them) names want.
func policyActionIncludes(action interface{}, want string) bool {
	switch a := action.(type) {
	case string:
		return a == want
	case []interface{}:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	case []string:
		for _, s := range a {
			if s == want {
				return true
			}
		}
	}
	return false
}
