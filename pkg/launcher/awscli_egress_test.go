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
// fetches from must be on it. Since the #1494 mirror was retired (#1515) that
// is its only source, so a no-NAT --no-public-ip launch reaches it only
// through the proxy.
func TestAWSCLIOriginsAreOnTheEgressAllowlist(t *testing.T) {
	origins := 0
	for _, raw := range regexp.MustCompile(`https://[A-Za-z0-9.$-]+`).FindAllString(awsCLIInstallTemplate, -1) {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if strings.Contains(u.Host, "$") {
			t.Errorf("spawn_need_aws fetches from a templated host %s: the per-region mirror is retired (#1515)", u.Host)
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
