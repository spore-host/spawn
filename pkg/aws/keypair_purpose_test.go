package aws

import (
	"testing"

	"github.com/spore-host/spawn/pkg/keypairsweep"
)

// A key a --nested-spawn head imports is a session key (#1621), the kind the
// reaper and `spawn cleanup --session-keys` delete once unused; every other
// caller's is a workstation key, which neither touches.
func TestKeyPairPurposeFor(t *testing.T) {
	nested := "arn:aws:sts::435415984226:assumed-role/" + NestedSpawnRoleName() + "/i-0123456789abcdef0"
	for arn, want := range map[string]string{
		nested:                                 keypairsweep.PurposeSession,
		"arn:aws:iam::435415984226:user/alice": keypairsweep.PurposeWorkstation,
		"arn:aws:sts::435415984226:assumed-role/AWSReservedSSO_Admin_x/alice@example": keypairsweep.PurposeWorkstation,
		// An instance under any other role (a dev box with an instance profile)
		// keeps its key: only a nested head's key dies with its instance.
		"arn:aws:sts::435415984226:assumed-role/spored-instance-role/i-0123456789abcdef0": keypairsweep.PurposeWorkstation,
		"": keypairsweep.PurposeWorkstation,
	} {
		if got := keyPairPurposeFor(arn); got != want {
			t.Errorf("keyPairPurposeFor(%q) = %q, want %q", arn, got, want)
		}
	}
}
