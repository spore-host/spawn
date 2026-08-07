// Package ecrref parses private-ECR image references. It is a dependency-free
// leaf (stdlib only) so both pkg/taskproto (which must not import cmd) and
// pkg/userdata (which must not import cmd or taskproto) can share ONE parser
// instead of three independent copies of the same regex, each editable without
// the others noticing (found while fixing spore-host#353: the third copy was
// about to become a fourth divergence point).
package ecrref

import (
	"regexp"
	"strings"
)

// accountRe extracts the account ID and region from a private-ECR image host:
//
//	<account>.dkr.ecr.<region>.amazonaws.com/<repo>[:tag]
var accountRe = regexp.MustCompile(`^(\d{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com/`)

// Account returns the AWS account that owns a private-ECR image, or "" if the
// image isn't a private-ECR ref (e.g. a public docker.io/quay.io image).
func Account(image string) string {
	m := accountRe.FindStringSubmatch(image)
	if m == nil {
		return ""
	}
	return m[1]
}

// RegistryHost returns the registry host (everything before the first '/') of
// an image ref — the argument `docker login` expects.
func RegistryHost(image string) string {
	if i := strings.IndexByte(image, '/'); i >= 0 {
		return image[:i]
	}
	return image
}

// AuthHost returns the registry host and region to authenticate against for a
// private-ECR image, or ok=false if image isn't a private-ECR ref (a public
// image needs no login). The image's OWN embedded region wins over
// fallbackRegion when both are known — a cross-region ECR pull must
// authenticate against its own region, not the launching instance's.
func AuthHost(image, fallbackRegion string) (host, region string, ok bool) {
	m := accountRe.FindStringSubmatch(image)
	if m == nil {
		return "", "", false
	}
	region = fallbackRegion
	if m[2] != "" {
		region = m[2]
	}
	return RegistryHost(image), region, true
}
