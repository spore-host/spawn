package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestKeyNameOrNil is the #130 regression guard: an empty KeyName must become a
// nil pointer (field omitted from RunInstances) so EC2 doesn't reject the
// headless / SSM-only launch with "Invalid value ” for keyPairNames".
func TestKeyNameOrNil(t *testing.T) {
	if got := keyNameOrNil(""); got != nil {
		t.Errorf("empty KeyName must yield nil (omit the field), got %q", aws.ToString(got))
	}
	if got := keyNameOrNil("my-key"); aws.ToString(got) != "my-key" {
		t.Errorf("non-empty KeyName must pass through, got %q", aws.ToString(got))
	}
}
