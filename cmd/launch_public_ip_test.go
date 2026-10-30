package cmd

import (
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
)

// #1443: a --no-public-ip launch has no public IP to look up, so step 9 makes
// no lookup; any other launch looks it up once.
func TestLaunchedPublicIPSkipsTheLookupForNoPublicIP(t *testing.T) {
	for _, noPublic := range []bool{true, false} {
		lookups := 0
		ip, err := launchedPublicIP(progress.NewQuietProgress(), &aws.LaunchConfig{NoPublicIP: noPublic}, func() (string, error) {
			lookups++
			return "198.51.100.7", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want, wantIP := 1, "198.51.100.7"
		if noPublic {
			want, wantIP = 0, ""
		}
		if lookups != want || ip != wantIP {
			t.Errorf("NoPublicIP=%v: %d lookups, ip %q; want %d, %q", noPublic, lookups, ip, want, wantIP)
		}
	}
}
