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
		// outside the reaper's scan.
		map[string]interface{}{
			"Sid":    "NestedSpawnRunTaggedInstances",
			"Effect": "Allow",
			"Action": "ec2:RunInstances",
			"Resource": []interface{}{
				"arn:aws:ec2:*:" + acct + ":instance/*",
				"arn:aws:ec2:*:" + acct + ":volume/*",
			},
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
		// Lifecycle of spawn-managed resources only.
		map[string]interface{}{
			"Sid":    "NestedSpawnManageManaged",
			"Effect": "Allow",
			"Action": []interface{}{
				"ec2:StartInstances",
				"ec2:StopInstances",
				"ec2:TerminateInstances",
				"ec2:CancelSpotInstanceRequests",
				"ec2:GetPasswordData",
				"ec2:AuthorizeSecurityGroupIngress",
				"ec2:AuthorizeSecurityGroupEgress",
				"ec2:DeletePlacementGroup",
			},
			"Resource":  "*",
			"Condition": managedResource,
		},
		// `spawn connect` (Session Manager, through the aws CLI) and the
		// terminate-time task flush (SendCommand) — to spawn-managed instances.
		// The condition key is SSM's, because the action is SSM's.
		map[string]interface{}{
			"Sid":      "NestedSpawnSSMToManagedInstances",
			"Effect":   "Allow",
			"Action":   []interface{}{"ssm:SendCommand", "ssm:StartSession"},
			"Resource": "arn:aws:ec2:*:" + acct + ":instance/*",
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{"ssm:resourceTag/" + spawnManagedTag: "true"},
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
		// PassRole to EC2 only, and only the roles spawn makes: the shared
		// default identity and the per-task family (which includes this role, so
		// a child may itself be nested).
		map[string]interface{}{
			"Sid":    "NestedSpawnPassSpawnRoles",
			"Effect": "Allow",
			"Action": "iam:PassRole",
			"Resource": []interface{}{
				"arn:aws:iam::" + acct + ":role/" + SporedRoleName,
				"arn:aws:iam::" + acct + ":role/spawn-instance-*",
			},
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{"iam:PassedToService": "ec2.amazonaws.com"},
			},
		},
	}
}

// nestedSpawnPolicyDoc is the grant document, still carrying
// sporedAccountPlaceholder (bound by sporedPolicyDoc before it is written).
func nestedSpawnPolicyDoc() string {
	b, err := json.Marshal(map[string]interface{}{
		"Version":   "2012-10-17",
		"Statement": nestedSpawnStatements(),
	})
	if err != nil {
		panic(err) // static content; cannot fail
	}
	return string(b)
}

// NestedSpawnRoleName is the role and instance-profile name --nested-spawn
// creates or reuses: NestedSpawnRolePrefix plus 8 hex digits of the grant
// document's hash. No AWS call; usable by --dry-run.
func NestedSpawnRoleName() string {
	sum := sha256.Sum256([]byte(nestedSpawnPolicyDoc()))
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
// reaper-control-tag Deny does not apply to tag-on-create.
//
// The Deny stops an instance writing spawn:managed, spawn:launch-time,
// spawn:pre-stop and the rest onto an EXISTING resource
// (GHSA-8q3p-qcv8-2pr4, GHSA-c93q-4jh7-6pvj). IAM evaluates the tags of a
// RunInstances request as ec2:CreateTags too, so unchanged it would refuse
// every child launch: a child must be created carrying exactly those keys.
// ec2:CreateAction is present only in that create-time evaluation, so requiring
// it to be absent keeps the Deny whole for every existing resource. Only the
// nested role gets this variant; the default baseline is unchanged.
func (c *Client) nestedSpawnBaselinePolicyDoc() string {
	statements := c.sporedSelfManagementStatements()
	for i, st := range statements {
		m, ok := st.(map[string]interface{})
		if !ok || m["Effect"] != "Deny" {
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
	}, c.nestedSpawnBaselinePolicyDoc())
}
