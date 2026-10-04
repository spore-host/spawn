package aws

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

// iamAPIError builds an error shaped like the SDK's modeled IAM errors, so the
// predicates are exercised through iamErrorCode (errors.As on smithy.APIError)
// rather than only the message fallback.
func iamAPIError(code, msg string) error {
	return &smithy.GenericAPIError{Code: code, Message: msg}
}

// concurrentModificationError reproduces what IAM actually returns when two
// tagged-resource creations race — verbatim from the spawn#648 report.
func concurrentModificationError() error {
	return iamAPIError("ConcurrentModification",
		"The previous tagging operation is still ongoing. Please wait for a while and "+
			"perform the next tagging operation until it finishes.")
}

// TestRetryIAMRetriesConcurrentModification is spawn#648.
//
// Two `spawn task run` invocations ~60ms apart (a Nextflow executor fanning out
// two processes): one succeeded, the other died at CreateInstanceProfile with
// ConcurrentModification and took the whole DAG with it. The call was already
// wrapped in retryIAM — the gap was the predicate. ConcurrentModification is
// neither "already exists" nor throttling, so retryIAM returned on the FIRST
// attempt without ever sleeping, which is the opposite of what IAM asks for.
func TestRetryIAMRetriesConcurrentModification(t *testing.T) {
	calls := 0
	err := retryIAM(func() error {
		calls++
		if calls < 3 {
			return concurrentModificationError()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryIAM should converge once the tagging operation drains, got %v", err)
	}
	if calls != 3 {
		t.Errorf("fn called %d times, want 3 — ConcurrentModification must be retried, not "+
			"returned on the first attempt", calls)
	}
}

func TestIsConcurrentModification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"modeled code", concurrentModificationError(), true},
		{"message-only fallback (emulator / wrapped)",
			errors.New("operation error IAM: CreateInstanceProfile, ConcurrentModification: busy"), true},
		{"nil", nil, false},
		{"throttling is a different predicate", iamAPIError("Throttling", "slow down"), false},
		{"already exists is a different predicate", iamAPIError("EntityAlreadyExists", "exists"), false},
		{"unrelated", iamAPIError("AccessDenied", "nope"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConcurrentModification(tc.err); got != tc.want {
				t.Errorf("isConcurrentModification = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRetryIAMStillFailsFastOnRealErrors: the fix must not turn every IAM error
// into a 7.5-second retry loop. AccessDenied is the case that matters — a
// misconfigured policy should surface immediately, not after five sleeps.
func TestRetryIAMStillFailsFastOnRealErrors(t *testing.T) {
	calls := 0
	err := retryIAM(func() error {
		calls++
		return iamAPIError("AccessDenied", "not authorized")
	})
	if err == nil {
		t.Fatal("AccessDenied must not be swallowed")
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want 1 — a non-retryable error must fail fast", calls)
	}
}

// TestRetryIAMGivesUpAndReturnsTheError: a tagging operation that never drains
// must surface the real error rather than a nil or a generic timeout.
func TestRetryIAMGivesUpAndReturnsTheError(t *testing.T) {
	// Exhausting all 5 attempts means sleeping the full 500+1000+1500+2000ms
	// backoff. CI runs -short, and TestRetryIAMRetriesConcurrentModification
	// already proves the retry happens there in 1.5s.
	if testing.Short() {
		t.Skip("skipping the full 7.5s backoff in -short mode")
	}
	calls := 0
	err := retryIAM(func() error {
		calls++
		return concurrentModificationError()
	})
	if err == nil {
		t.Fatal("persistent ConcurrentModification must eventually be returned, not swallowed")
	}
	if !isConcurrentModification(err) {
		t.Errorf("the returned error should be the last real one, got %v", err)
	}
	if calls != 5 {
		t.Errorf("fn called %d times, want 5 attempts", calls)
	}
}

// TestEveryTaggedIAMCreateIsRetried is a drift gate, and the reason this fix is
// worth more than one line.
//
// SetupSporedIAMRole reimplemented the #64 hardening by hand — CreateRole,
// CreateInstanceProfile and AddRoleToInstanceProfile with no retryIAM, tolerating
// only EntityAlreadyExists/LimitExceeded by string match. That is the same
// concern solved twice, so a fix to one did not reach the other. All of those
// calls pass Tags, which is exactly what makes IAM serialise and return
// ConcurrentModification.
//
// Asserts structurally that no CreateRole / CreateInstanceProfile /
// AddRoleToInstanceProfile call sits outside a retryIAM closure, so a newly
// added one cannot quietly reintroduce the gap.
func TestEveryTaggedIAMCreateIsRetried(t *testing.T) {
	src, err := os.ReadFile("iam.go")
	if err != nil {
		t.Fatalf("read iam.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")

	call := regexp.MustCompile(`iamClient\.(CreateRole|CreateInstanceProfile|AddRoleToInstanceProfile)\(`)
	found := 0
	for i, line := range lines {
		m := call.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		found++
		// Inside a retryIAM closure the call's result is bound to the closure's
		// error (`_, e :=`); a bare call assigns to the enclosing `err`.
		if !strings.Contains(line, "_, e :=") {
			t.Errorf("iam.go:%d: %s is not inside a retryIAM closure.\n"+
				"It passes Tags, so two concurrent launches race on the implicit tagging "+
				"operation and IAM returns ConcurrentModification (spawn#648). Wrap it in "+
				"retryIAM rather than hand-rolling the tolerance.", i+1, m[1])
		}
	}
	if found < 6 {
		t.Fatalf("expected to find the IAM create call sites, found %d — has the shape changed?", found)
	}
}
