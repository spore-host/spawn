// Package reaperiam holds the IAM permissions the ttl-reaper Lambda needs when it
// scans its OWN account (REAPER_SCAN_SELF), as data.
//
// It is one definition shared by whatever deploys the reaper and by the drift test
// that checks the policy against the reaper's own AWS calls. That sharing is the
// point: a policy maintained separately from the code that calls the APIs drifts, and
// every instance of that drift in this suite has been a silent failure —
// spawn#622 (a missing iam:CreateServiceLinkedRole left --fsx-import-path mounting an
// empty 1200 GiB filesystem while the CLI reported success), and lagotto #149/#151/
// #153 (three successive missing grants, each found only when a watch died).
//
// # The gap this package closes
//
// lambda/ttl-reaper/template.yaml grants its function sts:AssumeRole plus conditional
// ec2:DescribeInstances/DescribeTags/TerminateInstances — and NO fsx: actions at all.
// The reaper's fsxAPI calls DescribeFileSystems and DeleteFileSystem (the #210
// ephemeral-orphan net). In CROSS-ACCOUNT mode those run under the assumed
// spawn-ttl-reaper-ec2 role, which does grant them (verified against the live role in
// a spore-launching account). In SCAN-SELF mode they run under the Lambda's own role,
// which does not — so a reaper deployed to scan its own account could terminate
// instances but would silently fail to delete the orphaned filesystem that #613 was
// reported for. Scan-self is exactly the mode a self-hosted reaper uses.
//
// # The other two copies
//
// The SAM template's ScanSelf branch and the cross-account role
// (deployment/cloudformation/ttl-reaper-cross-account-role.yaml) are YAML and cannot
// import this package, so they are copies. template_drift_test.go parses both and
// fails unless every statement equals one here in actions, resources and conditions
// (#985, after the SAM copy was found missing the #685 grants and still granting
// ssm:SendCommand unconditioned). Change this file first, then mirror it.
package reaperiam

import (
	"encoding/json"
	"fmt"
)

// ManagedTagCondition scopes a destructive action to resources spawn owns. Every
// action that destroys something carries it: the reaper runs in an account that may
// hold resources spawn knows nothing about, and an unconditioned TerminateInstances
// there is an outage waiting for a tagging mistake.
var ManagedTagCondition = map[string]map[string]string{
	"StringEquals": {"ec2:ResourceTag/spawn:managed": "true"},
}

// ManagedSSMTagCondition is the same rail for ssm:* actions.
//
// It exists separately because a condition key belongs to the service of the
// ACTION, not of the resource. ec2:ResourceTag is an EC2 key and is simply absent
// from the request context of an ssm: call, so reusing ManagedTagCondition on
// ssm:SendCommand would produce a statement IAM accepts and never authorizes — a
// silent implicit DENY, which is the #652/#665 shape this package already
// documents twice.
var ManagedSSMTagCondition = map[string]map[string]string{
	"StringEquals": {"ssm:resourceTag/spawn:managed": "true"},
}

// ManagedIAMTagCondition is the same rail for iam: actions (#1300): IAM's own
// service key, for the same reason as ManagedSSMTagCondition.
var ManagedIAMTagCondition = map[string]map[string]string{
	"StringEquals": {"iam:ResourceTag/spawn:managed": "true"},
}

// InstanceRoleARN and InstanceProfileARN are the only IAM resources the reaper
// may change: spawn's per-task spawn-instance-<hash> roles and instance
// profiles (pkg/instancerole.NamePrefix). Neither matches the shared
// spored-instance-role / spored-instance-profile, the reaper's own role, or
// any role a user made. The account is '*' because a policy can only ever act
// in its own account; ${aws:PrincipalAccount} in a Resource is rejected by
// PutRolePolicy (#1069).
const (
	InstanceRoleARN    = "arn:aws:iam::*:role/spawn-instance-*"
	InstanceProfileARN = "arn:aws:iam::*:instance-profile/spawn-instance-*"
)

// Statement is the minimal IAM statement shape emitted here.
type Statement struct {
	Effect    string      `json:"Effect"`
	Action    []string    `json:"Action"`
	Resource  string      `json:"Resource"`
	Condition interface{} `json:"Condition,omitempty"`
}

// ScanSelfStatements are the permissions the reaper needs against its own account.
//
// Keep this in step with the reaper's *API interfaces in lambda/ttl-reaper/main.go —
// TestPolicyCoversEveryReaperAPICall enforces it, so adding an AWS call without a
// grant fails the build rather than failing silently at 03:00 in someone's account.
func ScanSelfStatements() []Statement {
	return []Statement{
		{
			// Finding expired instances. Read-only, so unconditioned: the reaper has
			// to see every instance to decide which are spawn's.
			//
			// NOT ec2:DescribeTags, which the CFN template and the cross-account role
			// both used to grant (removed in #985): the reaper reads tags from the DescribeInstances response
			// (tagMap(inst.Tags)) and never calls DescribeTags. Harmless there, but
			// an unused grant is a leftover, and
			// TestPolicyGrantsNothingTheReaperDoesNotCall keeps this list honest in
			// both directions.
			Effect:   "Allow",
			Action:   []string{"ec2:DescribeInstances"},
			Resource: "*",
		},
		{
			// The destructive one. Conditioned on spawn:managed=true — this is the
			// rail that kept the reaper away from three unrelated 1200 GiB research
			// filesystems and a c7g.4xlarge belonging to someone else in a shared
			// account.
			Effect:    "Allow",
			Action:    []string{"ec2:TerminateInstances"},
			Resource:  "*",
			Condition: ManagedTagCondition,
		},
		{
			// Cancelled before every terminate (#922): a persistent Spot request
			// left open launches a replacement for the instance just reaped. On
			// the same rail: spawn tags a persistent request with the instance's
			// own tags, spawn:managed=true included, so the condition matches the
			// requests spawn made and nothing else. An untagged (pre-#922,
			// one-time) request is refused, which is harmless — a one-time request
			// closes with its instance.
			Effect:    "Allow",
			Action:    []string{"ec2:CancelSpotInstanceRequests"},
			Resource:  "*",
			Condition: ManagedTagCondition,
		},
		{
			// The orphaned persistent Spot request sweep (#950). Read-only, and
			// DescribeSpotInstanceRequests takes no resource-level ARN. What it
			// leads to, cancelling a request and terminating a replacement, is
			// the two tag-conditioned grants above. So an UNTAGGED replacement is
			// deliberately not terminable by the reaper. It is reported instead.
			//
			// #984 looked for a safe way to widen that and found none. The EC2
			// instance resource has no condition key naming its Spot request, and
			// aws:ResourceTag on TerminateInstances or CreateTags is the
			// instance's own tags, so neither grant can be limited to "instances
			// a spawn-tagged request launched". Dropping the tag condition would
			// let the reaper end instances spawn does not own. A person ends the
			// replacement, from the SPOT REPLACEMENT NOT TERMINATED line.
			Effect:   "Allow",
			Action:   []string{"ec2:DescribeSpotInstanceRequests"},
			Resource: "*",
		},
		{
			// Network resources (#685): the security-group / placement-group
			// sweep. Read-only; neither Describe supports resource-level
			// permissions, so Resource must be "*".
			Effect: "Allow",
			Action: []string{
				"ec2:DescribeSecurityGroups",
				"ec2:DescribePlacementGroups",
				// The orphaned launch Elastic IP sweep (#661). DescribeAddresses
				// has no resource-level permissions either.
				"ec2:DescribeAddresses",
				// The per-task IAM role sweep (#1300) checks every region
				// enabled in the account for an instance holding a role's
				// profile, not only the scanned ones. Read-only.
				"ec2:DescribeRegions",
			},
			Resource: "*",
		},
		{
			// The destructive half of the #685 sweep, tag-gated on the same rail
			// as TerminateInstances.
			//
			// ec2:ResourceTag is the SERVICE key and both of these are ec2:
			// actions, so it is the right key here — unlike #665, where an
			// ec2:ResourceTag condition on an ssm: action produced a silent
			// implicit deny that looked like a working policy.
			//
			// Without these two grants the sweep would hit AccessDenied on every
			// cycle. That is the #652/#665 shape: a policy IAM accepts and does
			// not authorize, which is invisible until something quietly stops
			// happening — so both are simulated in BOTH directions before
			// shipping, not merely applied.
			Effect: "Allow",
			Action: []string{
				"ec2:DeleteSecurityGroup",
				"ec2:DeletePlacementGroup",
				// Release a launch Elastic IP whose instance is gone (#661). On
				// the same tag rail: an Elastic IP the user allocated carries no
				// spawn:managed=true and is denied here, not merely skipped.
				"ec2:ReleaseAddress",
				// CreateTags, so the sweep can stamp an UNTAGGED spawn-managed
				// resource with its first-seen time and age it out on a later
				// cycle. Without this the stamp fails and the resource is skipped
				// forever — which is the hole that left 21 orphaned security
				// groups uncollectable after the reaper shipped.
				//
				// Tag-conditioned like the deletes, so the reaper can only stamp
				// something already carrying spawn:managed=true. It cannot adopt a
				// resource that is not ours by tagging it.
				"ec2:CreateTags",
			},
			Resource:  "*",
			Condition: ManagedTagCondition,
		},
		{
			// The empty-bucket sweep (#1301), one statement per bucket family.
			// The ARN prefix is the IAM rail: no other bucket in the account
			// can be listed, retagged or deleted with these. The rest of the
			// guard is the sweep's own checks (pkg/bucketsweep): it deletes
			// only an empty, spawn-tagged bucket named exactly for the account
			// and region, every call naming the account as the expected owner.
			//
			// The staging bucket gets no list action at all: no role spawn
			// makes may list it (#1274), so the sweep never does, and S3's
			// refusal to delete a non-empty bucket is its emptiness check.
			Effect:   "Allow",
			Action:   StagingBucketSweepActions(),
			Resource: "arn:aws:s3:::spawn-staging-*",
		},
		{
			Effect:   "Allow",
			Action:   ResultsBucketSweepActions(),
			Resource: "arn:aws:s3:::spawn-results-*",
		},
		{
			// FSx: the #210 ephemeral-orphan net. DescribeFileSystems is read-only.
			//
			// DeleteFileSystem is NOT tag-conditioned here, unlike ec2:TerminateInstances
			// above, so the reaper's own in-code check (it only deletes a filesystem
			// tagged spawn:managed with no live lease, past a grace period) is the
			// only guard on this path. Worth stating plainly rather than implying an
			// IAM rail covers it.
			//
			// Two condition keys are easy to conflate here (#652). FSx exposes no
			// SERVICE key fsx:ResourceTag for this action — but the GLOBAL key
			// aws:ResourceTag/${TagKey} does work, and the cross-account role in
			// deployment/cloudformation/ttl-reaper-cross-account-role.yaml uses it.
			// Confirmed by IAM policy simulation: with the tag present and "true" the
			// call is `allowed`; absent, or present and "false", it is `implicitDeny`.
			//
			// Deliberately NOT adopted here yet. The simulation proves IAM EVALUATES
			// the key; it cannot prove FSx POPULATES it at request time, because the
			// simulator takes that context from the caller. If FSx does not supply it,
			// the condition fails closed — deletes denied, filesystems leak, which is
			// the safe direction but still a regression. Cross-account already carries
			// that dependency; adding it to scan-self would extend an unverified one
			// to the path #625 exists to make work. Resolve it with a real
			// ephemeral-FSx reap (see #613) before tightening this.
			Effect:   "Allow",
			Action:   []string{"fsx:DescribeFileSystems", "fsx:DeleteFileSystem"},
			Resource: "*",
		},
		{
			// The REAPER_GRACEFUL path asks spored to shut down cleanly over SSM
			// before an external terminate.
			//
			// Scoped to spawn-managed instances (#818). This was
			// `Resource: "*"` with no condition, which granted arbitrary shell as
			// root on ANY SSM-managed instance in the account — the reaper invokes
			// DocumentName "AWS-RunShellScript" (main.go:1029) — and so strictly
			// dominated the ec2:TerminateInstances that this same document
			// tag-gates with considerable care.
			//
			// The cross-account role in
			// deployment/cloudformation/ttl-reaper-cross-account-role.yaml:173-197
			// has had the correct shape all along; the scan-self copy never
			// received it. This is that shape, including the split below: the
			// comment that used to sit here claimed neither policy granted these
			// at all, which stopped being true when the cross-account grant landed.
			Effect:    "Allow",
			Action:    []string{"ssm:SendCommand"},
			Resource:  "arn:aws:ec2:*:*:instance/*",
			Condition: ManagedSSMTagCondition,
		},
		{
			// SendCommand authorizes against the instance AND the document, and a
			// document carries no spawn:managed tag — so conditioning this
			// statement too would deny every call. Scoped instead to the single
			// document the reaper actually names.
			Effect:   "Allow",
			Action:   []string{"ssm:SendCommand"},
			Resource: "arn:aws:ssm:*:*:document/AWS-RunShellScript",
		},
		{
			// GetCommandInvocation takes no resource-level ARN, so Resource "*" is
			// the only expressible form. It only reads the result of a command
			// this role already sent.
			Effect:   "Allow",
			Action:   []string{"ssm:GetCommandInvocation"},
			Resource: "*",
		},
		{
			// The per-task IAM role sweep (#1300, pkg/instancerole). ListRoles
			// has no resource-level ARN; it returns names only (no tags), and
			// everything the reaper does with a name is in the grants below.
			Effect:   "Allow",
			Action:   []string{"iam:ListRoles"},
			Resource: "*",
		},
		{
			// Reading one candidate role: its tags, creation date and
			// RoleLastUsed (GetRole), the profiles it is in, and its policies so
			// they can be removed first. Read-only, and unconditioned on
			// purpose: the reaper has to read an untagged role to learn that it
			// must keep it.
			Effect: "Allow",
			Action: []string{
				"iam:GetRole",
				"iam:ListInstanceProfilesForRole",
				"iam:ListRolePolicies",
				"iam:ListAttachedRolePolicies",
			},
			Resource: InstanceRoleARN,
		},
		{
			Effect:   "Allow",
			Action:   []string{"iam:GetInstanceProfile"},
			Resource: InstanceProfileARN,
		},
		{
			// The deletes, contained twice: by name to spawn-instance-* roles,
			// and by tag to ones spawn marked spawn:managed=true. So the shared
			// spored-instance-role, the reaper's own role and every role a user
			// made are denied by IAM, not merely skipped by code.
			//
			// RemoveRoleFromInstanceProfile appears here and in the profile
			// statement below. Its resource is the instance profile; granting
			// it on the role too, under the same rails, costs nothing and keeps
			// the sweep working if IAM also authorizes it against the role.
			Effect: "Allow",
			Action: []string{
				"iam:RemoveRoleFromInstanceProfile",
				"iam:DeleteRolePolicy",
				"iam:DetachRolePolicy",
				"iam:DeleteRole",
			},
			Resource:  InstanceRoleARN,
			Condition: ManagedIAMTagCondition,
		},
		{
			Effect: "Allow",
			Action: []string{
				"iam:RemoveRoleFromInstanceProfile",
				"iam:DeleteInstanceProfile",
			},
			Resource:  InstanceProfileARN,
			Condition: ManagedIAMTagCondition,
		},
	}
}

// StagingBucketSweepActions are what the empty-bucket sweep (#1301) needs on
// spawn-staging-*: read and write the bucket's tags, and delete it. Nothing
// that lists it or reads an object in it.
func StagingBucketSweepActions() []string {
	return []string{"s3:GetBucketTagging", "s3:PutBucketTagging", "s3:DeleteBucket"}
}

// ResultsBucketSweepActions are what the sweep needs on spawn-results-*, as
// IAM names. They differ from the SDK method names in five places: HeadBucket
// is s3:ListBucket, ListObjectVersions s3:ListBucketVersions,
// ListMultipartUploads s3:ListBucketMultipartUploads,
// GetBucketLifecycleConfiguration s3:GetLifecycleConfiguration, and
// DeleteBucketTagging is authorized as s3:PutBucketTagging. No object is read.
func ResultsBucketSweepActions() []string {
	return []string{
		"s3:ListBucket",
		"s3:ListBucketVersions",
		"s3:ListBucketMultipartUploads",
		"s3:GetBucketTagging",
		"s3:PutBucketTagging",
		"s3:GetLifecycleConfiguration",
		"s3:DeleteBucket",
	}
}

// ScanSelfPolicyDocument renders [ScanSelfStatements] as an IAM policy document.
func ScanSelfPolicyDocument() (string, error) {
	doc := map[string]interface{}{
		"Version":   "2012-10-17",
		"Statement": ScanSelfStatements(),
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("reaperiam: marshal policy: %w", err)
	}
	return string(b), nil
}

// GrantedActions is the flat set of actions [ScanSelfStatements] allows, for the
// drift test and for anything that needs to answer "can the reaper call X".
func GrantedActions() map[string]bool {
	out := map[string]bool{}
	for _, st := range ScanSelfStatements() {
		if st.Effect != "Allow" {
			continue
		}
		for _, a := range st.Action {
			out[a] = true
		}
	}
	return out
}
