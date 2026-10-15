package provider

import (
	"os"
	"testing"
)

// TestMain stubs the public-IP lookup NewLocalProvider makes when the local
// config names no public IP. The real one queries api.ipify.org, ifconfig.me and
// icanhazip.com; every NewLocalProvider test did, on every run (#968). The stub
// returns "" — what those tests already saw in CI, where the queries failed.
func TestMain(m *testing.M) {
	lookupPublicIP = func() string { return "" }
	os.Exit(m.Run())
}
