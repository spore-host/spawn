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
			// both grant: the reaper reads tags from the DescribeInstances response
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
			// Network resources (#685): the security-group / placement-group
			// sweep. Read-only; neither Describe supports resource-level
			// permissions, so Resource must be "*".
			Effect: "Allow",
			Action: []string{
				"ec2:DescribeSecurityGroups",
				"ec2:DescribePlacementGroups",
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
			// before an external terminate. Neither the template's function policy
			// nor the cross-account role grants these today, so graceful mode
			// degrades to an immediate terminate — quietly.
			Effect:   "Allow",
			Action:   []string{"ssm:SendCommand", "ssm:GetCommandInvocation"},
			Resource: "*",
		},
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
