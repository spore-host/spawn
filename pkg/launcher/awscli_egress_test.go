package launcher

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/egressproxy"
)

// The egress proxy's allowlist is the one list of what spawn's bootstrap
// fetches from outside AWS (#1515): every fixed origin the AWS CLI install
// fetches from must be on it. The regional spawn-binaries mirror is S3,
// reached through the endpoint --no-public-ip requires, not the proxy.
func TestAWSCLIOriginsAreOnTheEgressAllowlist(t *testing.T) {
	origins := 0
	for _, raw := range regexp.MustCompile(`https://[A-Za-z0-9.$-]+`).FindAllString(awsCLIInstallTemplate, -1) {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if strings.Contains(u.Host, "$") { // the per-region S3 mirror
			continue
		}
		origins++
		if !egressproxy.Allowed(u.Hostname()) {
			t.Errorf("%s, which spawn_need_aws fetches from, is not on egressproxy.Allowlist", u.Hostname())
		}
	}
	if origins == 0 {
		t.Fatal("found no fixed origin in awsCLIInstallTemplate; the test no longer checks anything")
	}
}
