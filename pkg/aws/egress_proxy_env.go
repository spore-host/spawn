package aws

import (
	"fmt"
	"net/netip"
	"strings"
)

// A member that counts on the spawn egress proxy (#1515 phase 4) sets
// http_proxy, https_proxy and no_proxy only around spawn's own fetches from
// outside AWS: the EFA installer's download and run, the Ubuntu apt-get calls
// spawn runs, and the AWS CLI zip's download. Never globally, and never
// for the user's --command or --mpi-command. These two functions are the one
// source of the proxy URL and the no_proxy list pkg/userdata and pkg/launcher
// render.

// EgressProxyURL is the proxy URL a member's scoped fetches use for addr, the
// proxy's private host:port ([LaunchConfig.EgressProxyAddr]). addr must be an
// IP address and port, which is all find-or-create produces: the URL is
// rendered into the user data unquoted, so nothing else is accepted.
func EgressProxyURL(addr string) (string, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsValid() || ap.Port() == 0 || ap.Addr().Zone() != "" {
		return "", fmt.Errorf("egress proxy address %q is not an IP address and port", addr)
	}
	return "http://" + ap.String(), nil
}

// EgressProxyNoProxy is the no_proxy of a member's proxied fetches in region:
// every host they can reach that is inside the instance or the VPC, so it
// never goes to the proxy, which allows only the hosts on
// egressproxy.Allowlist and would refuse it.
//
//   - IMDS and the loopback names.
//   - S3, in every form a fetch names it: the regional virtual-hosted
//     (spawn-binaries-<region>.s3.<region>.amazonaws.com: spored and the EFA
//     installer mirror) and path forms, the form AL2023's dnf repositories use, and
//     the global form. The gateway endpoint --no-public-ip requires serves
//     all of them. The AL2023 AMI's repositories come from its
//     amazon-linux-repo-s3 package (2023.12.20261021, read from the package):
//     mirrorlist=https://al2023-repos-$awsregion-de612dc2.s3$dualstack.$awsregion.$awsdomain/...,
//     with /etc/dnf/vars/dualstack ".dualstack" (or empty), so either
//     al2023-repos-<region>-de612dc2.s3.dualstack.<region>.amazonaws.com or
//     the same without ".dualstack". (The amazonlinux:2023 container image
//     ships amazon-linux-repo-cdn instead, cdn.amazonlinux.com; no EC2 member
//     uses it.)
//   - Every other regional AWS endpoint a private member is checked for
//     (privateServices: ec2, ssm, ssmmessages, ec2messages, dynamodb, fsx,
//     sqs, xray), as <service>.<region>.amazonaws.com. Those are served by the
//     VPC's interface or gateway endpoints. The bootstrap calls no STS.
//
// It is not a blanket ".amazonaws.com": efa-installer.amazonaws.com and
// awscli.amazonaws.com are outside AWS's VPC endpoints and must reach the
// proxy (TestEgressProxyNoProxySparesTheAllowlist).
func EgressProxyNoProxy(region string) []string {
	out := []string{
		"169.254.169.254", "localhost", "127.0.0.1",
		".s3.amazonaws.com",
		".s3." + region + ".amazonaws.com",
		".s3.dualstack." + region + ".amazonaws.com",
		"s3." + region + ".amazonaws.com",
	}
	for _, s := range privateServices {
		if s.Endpoint == "" || s.Endpoint == "s3" {
			continue
		}
		out = append(out, s.Endpoint+"."+region+".amazonaws.com")
	}
	return out
}

// EgressProxyEnv is the variable assignments, ready to prefix one shell
// command, that send that command's fetches through the proxy at addr in
// region: http_proxy, https_proxy and no_proxy. Every value is a plain token
// (an IP URL, host names, commas), so it needs no quoting.
func EgressProxyEnv(addr, region string) (string, error) {
	u, err := EgressProxyURL(addr)
	if err != nil {
		return "", err
	}
	if !validRegion(region) {
		return "", fmt.Errorf("egress proxy: region %q is not a region name", region)
	}
	return "http_proxy=" + u + " https_proxy=" + u + " no_proxy=" + strings.Join(EgressProxyNoProxy(region), ","), nil
}

// validRegion reports whether region is a plain region name (us-east-1),
// which the rendered no_proxy carries unquoted.
func validRegion(region string) bool {
	if region == "" {
		return false
	}
	for _, c := range region {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
