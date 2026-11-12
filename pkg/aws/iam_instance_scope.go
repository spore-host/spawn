package aws

import (
	"encoding/json"
	"strings"
)

// Self-scoped instance permissions (schema 2).
//
// Up to schema 1 the spored roles granted ec2:StopInstances,
// ec2:TerminateInstances, ec2:CreateTags and ec2:DeleteTags on any instance
// tagged spawn:managed=true. spored only ever stops, terminates or tags its
// own instance (pkg/provider, pkg/agent and cmd/spored all name
// identity.InstanceID), so the account-wide reach served nothing spored does,
// and any process able to read the role's credentials from IMDS could stop,
// terminate or retag every spawn-managed instance in the account.
//
// The condition. aws:ARN is the ARN of the resource the action is authorized
// against and ${ec2:SourceInstanceARN} is the ARN of the instance the calling
// credentials were issued to, so StringEquals between them holds only for a
// call about the caller itself. It is the form TerminateOnlySelf has used
// since #1080, proven on real AWS there (PutRolePolicy accepts it; a dry-run
// terminate of another instance was refused and of itself allowed). The
// variable sits in a Condition, never a Resource (#1069). ec2:SourceInstanceARN
// is present on every request signed with instance-profile credentials, and
// these roles are assumable by ec2.amazonaws.com only.
//
// The nested exception. A --nested-spawn head node manages the children it
// launched (pkg/aws nested_spawn.go), and the Denies below are shared with its
// baseline. A child carries NestedParentTagKey = the head's ${aws:userid}
// ("<role id>:<instance id>"), set at RunInstances (the nested grant requires
// it there) and in reaperControlTagKeys, so no instance can write it onto an
// existing resource. The Denies spare a resource carrying the caller's own
// value, which only the head's own children do.
//
// Rollout. CLIs up to v0.161.0 rewrite their unversioned spored-policy /
// spored-baseline-policy on every launch, with the old account-wide grants,
// and IAM grants the union of a role's policies. So removing the Allow is not
// enough while such a CLI launches in the account: the explicit Denies here
// are what retire those grants (iam_policy_schema.go, "Removing a grant").

// NestedParentTagKey is the tag a nested spore's child carries: the head
// node's ${aws:userid} ("<role unique id>:<instance id>"). Only the nested
// role's RunInstances may set it, and only to the caller's own value; no
// instance may write it afterwards (reaperControlTagKeys).
const NestedParentTagKey = "spawn:nested-parent"

// notSelfNorNestedChild is the condition, in its negated form, the Denies use:
// the resource is neither the calling instance nor a child the caller
// launched as a nested head. Keys within one operator are ANDed, and a
// negated operator is true when the key is absent (a resource with no
// nested-parent tag), so the Deny applies to everything else.
func notSelfNorNestedChild() map[string]interface{} {
	return map[string]interface{}{
		"StringNotEquals": map[string]interface{}{
			"aws:ARN":                               "${ec2:SourceInstanceARN}",
			"ec2:ResourceTag/" + NestedParentTagKey: "${aws:userid}",
		},
	}
}

// sporedInstanceScopeStatements are the spored roles' instance-mutating
// grants, every one of them limited to the calling instance:
//
//   - TagOnlySelf: CreateTags/DeleteTags on the caller's own instance (TTL,
//     heartbeat, cost footprint, DNS and FSx status tags), which also carries
//     spawn:managed=true. The reaper-control-tag Deny (reaperControlTagDeny)
//     still keeps the keys the reaper acts on out of the instance's reach.
//   - StopOnlySelf: StopInstances on the caller only (idle stop, hibernate,
//     on-complete stop). TerminateOnlySelf (sporedSelfTerminateStatement) is
//     the terminate half and is unchanged.
//   - CancelSpotInstanceRequests stays scoped to spawn:managed=true: a Spot
//     request is its own resource whose ARN is not the instance's, and it is
//     created before the instance, so no tag can name its instance at
//     creation. Cancelling a request never terminates an instance; it only
//     stops a persistent request launching a replacement (pkg/spotrequest).
//   - DenyTagOthers, DenyStopTerminateOthers: retire the account-wide grants
//     that older CLIs still write (see the comment at the top of this file).
func sporedInstanceScopeStatements() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"Sid":      "TagOnlySelf",
			"Effect":   "Allow",
			"Action":   []interface{}{"ec2:CreateTags", "ec2:DeleteTags"},
			"Resource": "*",
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{
					"aws:ARN":                       "${ec2:SourceInstanceARN}",
					"ec2:ResourceTag/spawn:managed": "true",
				},
			},
		},
		reaperControlTagDeny(),
		map[string]interface{}{
			"Sid":       "DenyTagOthers",
			"Effect":    "Deny",
			"Action":    []interface{}{"ec2:CreateTags", "ec2:DeleteTags"},
			"Resource":  "*",
			"Condition": notSelfNorNestedChild(),
		},
		map[string]interface{}{
			"Sid":      "StopOnlySelf",
			"Effect":   "Allow",
			"Action":   "ec2:StopInstances",
			"Resource": "*",
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{
					"aws:ARN": "${ec2:SourceInstanceARN}",
				},
			},
		},
		map[string]interface{}{
			"Sid":       "DenyStopTerminateOthers",
			"Effect":    "Deny",
			"Action":    []interface{}{"ec2:StopInstances", "ec2:TerminateInstances"},
			"Resource":  "*",
			"Condition": notSelfNorNestedChild(),
		},
		// ec2:CancelSpotInstanceRequests (#922): spored cancels its own
		// persistent Spot request before it self-terminates, or EC2 launches a
		// replacement. The request carries the instance's tags.
		map[string]interface{}{
			"Sid":      "CancelManagedSpotRequests",
			"Effect":   "Allow",
			"Action":   "ec2:CancelSpotInstanceRequests",
			"Resource": "*",
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{
					"ec2:ResourceTag/spawn:managed": "true",
				},
			},
		},
	}
}

// sporedInstanceScopeStatementsJSON renders them for the static DCV policy,
// so the two policies cannot drift on them.
func sporedInstanceScopeStatementsJSON() string {
	sts := sporedInstanceScopeStatements()
	parts := make([]string, 0, len(sts))
	for _, st := range sts {
		b, err := json.Marshal(st)
		if err != nil {
			panic(err) // static content; cannot fail
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, ",\n    ")
}
