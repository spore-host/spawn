package userdata

import (
	"net/url"
	"testing"

	"github.com/spore-host/spawn/pkg/egressproxy"
)

// The egress proxy's allowlist is the one list of what spawn's bootstrap
// fetches from outside AWS (#1515): the EFA installer's origin must be on it,
// or a --no-public-ip --efa member behind the proxy cannot install.
func TestEFAInstallerOriginIsOnTheEgressAllowlist(t *testing.T) {
	u, err := url.Parse(efaInstallerOrigin)
	if err != nil || u.Scheme != "https" {
		t.Fatalf("efaInstallerOrigin %q: %v", efaInstallerOrigin, err)
	}
	if !egressproxy.Allowed(u.Hostname()) {
		t.Errorf("%s (efaInstallerOrigin) is not on egressproxy.Allowlist", u.Hostname())
	}
}
