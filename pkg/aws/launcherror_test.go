package aws

import (
	"errors"
	"fmt"
	"testing"
)

// TestNewLaunchError_ExtractsCode verifies that an AWS API error in the chain
// surfaces its verbatim code on LaunchError.Code, so callers can classify the
// failure on a code rather than string-matching (#108).
func TestNewLaunchError_ExtractsCode(t *testing.T) {
	apiErr := &fakeAPIError{code: "InsufficientInstanceCapacity"}
	err := newLaunchError(apiErr)

	var le *LaunchError
	if !errors.As(err, &le) {
		t.Fatalf("newLaunchError did not produce a *LaunchError: %T", err)
	}
	if le.Code != "InsufficientInstanceCapacity" {
		t.Errorf("Code = %q, want InsufficientInstanceCapacity", le.Code)
	}
	// The original error must remain reachable via Unwrap so callers can still
	// errors.As(&smithyAPIError).
	if !errors.Is(err, apiErr) {
		t.Errorf("Unwrap chain lost the original API error")
	}
	if !contains(err.Error(), "InsufficientInstanceCapacity") {
		t.Errorf("Error() = %q, want it to mention the code", err.Error())
	}
}

// TestNewLaunchError_NonAPIError verifies that a plain error yields an empty
// Code (not a panic) and is still wrapped + unwrappable.
func TestNewLaunchError_NonAPIError(t *testing.T) {
	base := fmt.Errorf("dial tcp: connection refused")
	err := newLaunchError(base)

	var le *LaunchError
	if !errors.As(err, &le) {
		t.Fatalf("newLaunchError did not produce a *LaunchError: %T", err)
	}
	if le.Code != "" {
		t.Errorf("Code = %q, want empty for a non-API error", le.Code)
	}
	if !errors.Is(err, base) {
		t.Errorf("Unwrap chain lost the original error")
	}
}
