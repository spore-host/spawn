package aws

import (
	"testing"

	"github.com/spore-host/spawn/pkg/instancerole"
)

// pkg/instancerole (#1300) deletes what CreateOrGetInstanceProfile makes, and
// only that, so the two must agree on the name, the ownership tag and the
// shared identity it must never touch. Each of these drifting silently would
// either strand every role (nothing matches) or put the wrong one in reach.
func TestInstanceRoleDeleterAgreesWithTheCreator(t *testing.T) {
	name := PredictedInstanceRoleName(IAMRoleConfig{InlinePolicyJSON: `{"Version":"2012-10-17"}`})
	if !instancerole.IsCandidate(name) {
		t.Errorf("CreateOrGetInstanceProfile names a role %q, which pkg/instancerole would never consider", name)
	}
	if instancerole.IsCandidate(SporedRoleName) || instancerole.IsCandidate(SporedInstanceProfileName) {
		t.Error("the shared spored identity is a deletion candidate")
	}
	if instancerole.SharedRoleName != SporedRoleName || instancerole.SharedProfileName != SporedInstanceProfileName {
		t.Errorf("pkg/instancerole's shared names (%s, %s) differ from pkg/aws's (%s, %s)",
			instancerole.SharedRoleName, instancerole.SharedProfileName, SporedRoleName, SporedInstanceProfileName)
	}
	c := &Client{}
	found := false
	for _, tag := range c.buildIAMTags(nil) {
		if *tag.Key == instancerole.ManagedTagKey && *tag.Value == instancerole.ManagedTagValue {
			found = true
		}
	}
	if !found {
		t.Errorf("buildIAMTags does not write %s=%s, so no role spawn creates could ever be deleted",
			instancerole.ManagedTagKey, instancerole.ManagedTagValue)
	}
}
