package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/lifecyclemark"
)

// spawn#1666: spore.host provisions the lease permissions, so the idle
// teardown check's Warn names the policy `spawn setup-account` creates, its
// ARN in this account and every action it grants, rather than telling an
// administrator to write a statement by hand.
func TestLeaseUnavailableNamesTheProvisionedPolicy(t *testing.T) {
	msg := leaseUnavailable("123456789012", "arn:aws:iam::123456789012:user/u", errors.New("AccessDenied")).Error()
	for _, want := range append([]string{
		"arn:aws:iam::123456789012:user/u",
		"arn:aws:iam::123456789012:policy/" + lifecyclemark.UserPolicy,
		"spawn setup-account",
		lifecyclemark.TableARNPattern,
	}, lifecyclemark.UserActions()...) {
		if !strings.Contains(msg, want) {
			t.Errorf("the Warn does not say %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "does not provision") {
		t.Errorf("the Warn still says spore.host does not provision the lease permissions:\n%s", msg)
	}
}
