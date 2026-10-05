package aws

import "testing"

// TestOnlyMineIncludesUntaggedSharedInfrastructure is spawn#708.
//
// --mine filtered on spawn:iam-user, a tag pkg/aws/tags.go writes only to
// INSTANCES (and volumes, via the block-device mapping). Shared infrastructure —
// security groups, IAM roles and instance profiles, key pairs, log groups,
// DynamoDB tables — is created elsewhere and never gets it. A missing tag reads
// as "", which never equals a caller ARN, so for those classes the filter did
// not narrow the result: it EMPTIED it.
//
// Observed in the dev account, one flag apart:
//
//	$ spawn orphans --region us-east-1
//	No orphaned spawn-managed resources in us-east-1.
//	$ spawn orphans --region us-east-1 --all
//	21 resource(s)      # 9 security groups, 12 IAM profiles, oldest 2026-07-19
//
// The rule is now "not someone else's", not "provably mine".
func TestOnlyMineIncludesUntaggedSharedInfrastructure(t *testing.T) {
	const me = "arn:aws:iam::435415984226:user/scttfrdmn"
	const someoneElse = "arn:aws:iam::435415984226:user/colleague"

	tests := []struct {
		name string
		tags map[string]string
		want bool
		why  string
	}{
		{
			name: "untagged shared infrastructure is IN scope",
			tags: map[string]string{"spawn:managed": "true"},
			want: true,
			why: "this is the #708 case: every managed security group in the account has " +
				"NO spawn:iam-user, so excluding them made --mine report nothing, forever",
		},
		{
			name: "my own instance is in scope",
			tags: map[string]string{"spawn:iam-user": me},
			want: true,
		},
		{
			name: "another principal's resource is NOT in scope",
			tags: map[string]string{"spawn:iam-user": someoneElse},
			want: false,
			why:  "--mine must still mean something; cleanup DELETES what it reports",
		},
		{
			name: "an empty tag value is treated as untagged",
			tags: map[string]string{"spawn:iam-user": ""},
			want: true,
			why:  "a present-but-empty tag is indistinguishable from an absent one",
		},
		{
			name: "no tags at all",
			tags: nil,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownedByCaller(tt.tags, me); got != tt.want {
				t.Errorf("ownedByCaller(%v) = %v, want %v\n%s", tt.tags, got, tt.want, tt.why)
			}
		})
	}
}

// TestOnlyMineStillExcludesOtherPrincipals guards the over-correction. The fix
// must not turn --mine into --all: cleanup DELETES what discovery reports, so a
// colleague's security group must stay out of the default scope.
func TestOnlyMineStillExcludesOtherPrincipals(t *testing.T) {
	const me = "arn:aws:iam::435415984226:user/scttfrdmn"
	theirs := map[string]string{
		"spawn:managed":  "true",
		"spawn:iam-user": "arn:aws:iam::435415984226:role/someone-elses-role",
	}
	if ownedByCaller(theirs, me) {
		t.Error("a resource tagged for a different principal is in scope — --mine would be " +
			"equivalent to --all, and cleanup deletes what it reports")
	}

	// And the caller having no resolvable identity must not sweep in the world:
	// an empty callerARN would match every untagged resource AND, with a naive
	// equality, every empty tag. Untagged-is-shared is intentional; tagged-for-
	// someone-else must still be excluded even then.
	if ownedByCaller(theirs, "") {
		t.Error("with no caller identity, another principal's resource is still not mine")
	}
}

// TestRGTPlacementGroupRowsAreDropped is spawn#713.
//
// I asserted in #706 that the Resource Groups Tagging API "does not return
// placement groups at all", and built a dedicated scan on that premise. The
// claim was wrong — the probe it rested on ran against an account that had none
// at the time. RGT does return them, keyed by GROUP ID:
//
//	arn:aws:ec2:us-east-1:435415984226:placement-group/pg-040e64301b7f056f7
//
// Two bugs followed, both visible in a live `spawn cleanup --dry-run`:
//
//  1. Every managed group was reported TWICE — once by id with no state, once by
//     name with one. Six rows for three groups.
//  2. The id-keyed row was undeletable. EC2's DeletePlacementGroup takes a
//     GroupName, so passing pg-… fails InvalidPlacementGroup.Unknown — which
//     RemoveResource's ignoreNotFound then swallowed as SUCCESS. Cleanup
//     reported removing a group it had never touched.
//
// The dedicated scan wins because RGT supplies neither thing needed here: the
// name (required to delete) or whether the group still has members (required to
// call it an orphan at all).
func TestRGTPlacementGroupRowsAreDropped(t *testing.T) {
	if !supersededByDedicatedScan("ec2", "placement-group") {
		t.Error("RGT placement-group rows are kept, so every group is reported twice and " +
			"the id-keyed copy silently fails to delete (#713)")
	}

	// Nothing else may be dropped — suppressing a type that has no dedicated
	// scan would make real resources invisible, which is #708 all over again.
	for _, tc := range [][2]string{
		{"ec2", "security-group"}, {"ec2", "instance"}, {"ec2", "volume"},
		{"ec2", "key-pair"}, {"iam", "role"}, {"iam", "instance-profile"},
		{"logs", "log-group"}, {"dynamodb", "table"}, {"s3", ""},
	} {
		if supersededByDedicatedScan(tc[0], tc[1]) {
			t.Errorf("%s:%s is dropped from discovery but has no dedicated scan to replace "+
				"it — those resources would become invisible", tc[0], tc[1])
		}
	}
}

// TestSharedSporedIdentityIsNeverAnOrphan is the other half of spawn#713.
//
// IsLikelyOrphan's only signal for IAM is "is anything running", which says
// nothing about whether the SHARED spored role and instance profile are wanted —
// they are created once and reused by every launch. So an idle account made
// them orphans, and `spawn cleanup --yes` would delete spawn's own identity.
//
// This was latent until #708: the --mine scope filtered on spawn:iam-user, a tag
// these never carry, so they were invisible to cleanup by default and this could
// not fire. Fixing the scope unmasked it — the same change that surfaced 27 real
// orphans also offered these two for deletion in a live dry run. One bug was
// hiding the other.
func TestSharedSporedIdentityIsNeverAnOrphan(t *testing.T) {
	for _, id := range []string{SporedRoleName, SporedInstanceProfileName} {
		r := ManagedResource{Service: "iam", ResourceType: "role", ID: id}
		// Both with and without something running: the point is that the
		// hasRunningInstance signal is irrelevant for these.
		if IsLikelyOrphan(r, false) {
			t.Errorf("%s is an orphan in an idle account — 'spawn cleanup --yes' would delete "+
				"the shared identity every launch reuses (#713)", id)
		}
		if IsLikelyOrphan(r, true) {
			t.Errorf("%s is an orphan with instances running", id)
		}
		if !IsSharedSporedIdentity(r) {
			t.Errorf("IsSharedSporedIdentity(%s) = false; cleanup relies on this to skip it", id)
		}
	}

	// A PER-RUN role must still be reclaimable, or the guard would strand the
	// spawn-instance-<hash> profiles this is meant to leave alone (12 of them
	// were sitting in the dev account, the oldest three months old).
	perRun := ManagedResource{Service: "iam", ResourceType: "instance-profile", ID: "spawn-instance-b4848ca7"}
	if IsSharedSporedIdentity(perRun) {
		t.Error("a per-run spawn-instance-<hash> profile was treated as shared infrastructure")
	}
	if !IsLikelyOrphan(perRun, false) {
		t.Error("a per-run IAM profile is no longer an orphan in an idle account; the guard " +
			"over-reached and these would accumulate forever")
	}
}
