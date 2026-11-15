package reaperiam

import (
	"testing"

	"github.com/spore-host/spawn/pkg/keypairsweep"
)

// The reaper may delete only a session key pair (#1621): the grant's purpose
// is the sweep's, and the grant is the only DeleteKeyPair there is, so a
// user's workstation key (keypairsweep.PurposeWorkstation) is denied by IAM.
func TestDeleteKeyPairIsScopedToSessionKeys(t *testing.T) {
	if got := SessionKeyTagCondition["StringEquals"]["ec2:ResourceTag/spawn:purpose"]; got != keypairsweep.PurposeSession {
		t.Errorf("DeleteKeyPair grant is scoped to purpose %q, want %q", got, keypairsweep.PurposeSession)
	}
	if SessionKeyTagCondition["StringEquals"]["ec2:ResourceTag/spawn:managed"] != "true" {
		t.Error("DeleteKeyPair grant does not require spawn:managed=true")
	}
	grants := 0
	for _, st := range ScanSelfStatements() {
		for _, a := range st.Action {
			if a != "ec2:DeleteKeyPair" {
				continue
			}
			grants++
			cond, _ := st.Condition.(map[string]map[string]string)
			if st.Effect != "Allow" || cond["StringEquals"]["ec2:ResourceTag/spawn:purpose"] != keypairsweep.PurposeSession {
				t.Errorf("a DeleteKeyPair statement is not scoped to session keys: %+v", st)
			}
		}
	}
	if grants != 1 {
		t.Errorf("%d DeleteKeyPair statements, want exactly the one scoped to session keys", grants)
	}
}
