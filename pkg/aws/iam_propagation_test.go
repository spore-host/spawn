package aws

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// IAM eventual consistency must be waited out by POLLING, never by a blind
// sleep.
//
// waitForInstanceProfile exists precisely for this, and says so in its own doc
// comment — it "returns as soon as the profile is readable … instead of a blind
// fixed sleep". The blind sleep that comment describes was still in this file,
// 830 lines above it, for the life of both (#752).
//
// The only legitimate sleep here is retryIAM's scoped backoff: sub-second,
// attempt-scaled, and entered only for a throttle or a ConcurrentModification —
// i.e. a retry delay, not a propagation guess. Anything measured in SECONDS is
// the defect.
func TestNoBlindIAMPropagationSleep(t *testing.T) {
	b, err := os.ReadFile("iam.go")
	if err != nil {
		t.Fatalf("read iam.go: %v", err)
	}

	// A sleep whose duration is expressed in whole seconds. Matches
	// `10 * time.Second` and `time.Second`, not `500 * time.Millisecond`.
	secondsSleep := regexp.MustCompile(`time\.Sleep\([^)]*time\.Second`)

	var bad []string
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue // the fix's own comment quotes the old call
		}
		if secondsSleep.MatchString(line) {
			bad = append(bad, strings.TrimSpace(line)+"  (iam.go:"+itoa(i+1)+")")
		}
	}
	if len(bad) > 0 {
		t.Errorf("blind second-scale sleep(s) in iam.go:\n  %s\n"+
			"IAM propagation must be polled — use waitForInstanceProfile, or a bounded "+
			"loop around the read that needs to succeed. A sleep is both slower in the "+
			"common case and no guarantee in the slow one, and it ignores ctx.",
			strings.Join(bad, "\n  "))
	}

	// ...and the poll must still be there. A gate that only forbids the sleep
	// would pass if someone deleted the wait entirely.
	if !strings.Contains(string(b), "waitForInstanceProfile(ctx, iamClient, profileName)") {
		t.Error("nothing calls waitForInstanceProfile after attaching the role — the " +
			"profile may not be readable when GetInstanceProfile runs below it")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
