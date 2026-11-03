package aws

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/egressproxy"
)

// allowlistExamples turns each egressproxy.Allowlist pattern into hosts it
// matches: a one-label wildcard filled with a region and a short label.
func allowlistExamples(t *testing.T) []string {
	t.Helper()
	var hosts []string
	for _, r := range egressproxy.Allowlist {
		base, wild := strings.CutPrefix(r.Pattern, "*.")
		if !wild {
			hosts = append(hosts, r.Pattern)
			continue
		}
		hosts = append(hosts, "us-east-1."+base, "eu-west-2."+base, "x."+base)
	}
	for _, h := range hosts {
		if !egressproxy.Allowed(h) {
			t.Fatalf("example %s is not on the allowlist", h)
		}
	}
	return hosts
}

// no_proxy sends every S3 and VPC-endpoint host direct and every allowlisted
// host to the proxy, by curl's rule (#1515 phase 4). The proxy refuses
// anything off its allowlist, so an S3 name it is sent (AL2023's dnf
// repositories, the spawn-binaries mirror) breaks the member, and an
// allowlisted name sent direct cannot leave a no-NAT subnet.
func TestEgressProxyNoProxySparesTheAllowlist(t *testing.T) {
	for _, region := range []string{"us-east-1", "eu-west-2", "ap-southeast-4"} {
		noProxy := strings.Join(EgressProxyNoProxy(region), ",")
		for _, h := range allowlistExamples(t) {
			if egressproxy.NoProxyBypasses(noProxy, h) {
				t.Errorf("%s: no_proxy %q sends allowlisted %s direct; it must reach the proxy", region, noProxy, h)
			}
		}
		direct := []string{
			"169.254.169.254", "localhost", "127.0.0.1",
			// The spawn-binaries mirror of spored, the EFA installer and the AWS CLI.
			"spawn-binaries-" + region + ".s3." + region + ".amazonaws.com",
			// S3's path, dual-stack and global forms.
			"s3." + region + ".amazonaws.com",
			"s3.dualstack." + region + ".amazonaws.com",
			"bucket.s3.dualstack." + region + ".amazonaws.com",
			"bucket.s3.amazonaws.com",
			"s3.amazonaws.com",
			// AL2023's repositories, as the AMI's amazon-linux-repo-s3 names them,
			// with and without its $dualstack.
			"al2023-repos-" + region + "-de612dc2.s3.dualstack." + region + ".amazonaws.com",
			"al2023-repos-" + region + "-de612dc2.s3." + region + ".amazonaws.com",
		}
		for _, s := range PrivateServices() {
			if s.Endpoint != "" {
				direct = append(direct, s.Endpoint+"."+region+".amazonaws.com")
			}
		}
		for _, h := range direct {
			if !egressproxy.NoProxyBypasses(noProxy, h) {
				t.Errorf("%s: no_proxy %q sends %s to the proxy, which refuses it", region, noProxy, h)
			}
		}
		for _, e := range EgressProxyNoProxy(region) {
			if e == "amazonaws.com" || e == ".amazonaws.com" || e == "*" || strings.TrimPrefix(e, ".") == region+".amazonaws.com" {
				t.Errorf("%s: no_proxy entry %q is a blanket", region, e)
			}
		}
	}
}

func TestEgressProxyEnv(t *testing.T) {
	got, err := EgressProxyEnv("10.0.1.5:3128", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	want := "http_proxy=http://10.0.1.5:3128 https_proxy=http://10.0.1.5:3128 no_proxy=" + strings.Join(EgressProxyNoProxy("us-east-1"), ",")
	if got != want {
		t.Errorf("EgressProxyEnv = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "'\"$`;&|<>(){}\\\n") {
		t.Errorf("EgressProxyEnv %q holds a shell metacharacter; it is rendered unquoted", got)
	}
	for _, bad := range []string{"", "proxy:3128", "10.0.1.5", "10.0.1.5:0", "10.0.1.5:3128;id", "[fe80::1%eth0]:3128"} {
		if _, err := EgressProxyEnv(bad, "us-east-1"); err == nil {
			t.Errorf("EgressProxyEnv(%q) was accepted", bad)
		}
	}
	for _, bad := range []string{"", "us-east-1 x", "us-east-1;id", "US-EAST-1"} {
		if _, err := EgressProxyEnv("10.0.1.5:3128", bad); err == nil {
			t.Errorf("EgressProxyEnv region %q was accepted", bad)
		}
	}
}
