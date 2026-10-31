// Package s3hosts answers one question for the repo's gates: does a script
// spawn generates reach S3 by any name other than the launch region's regional
// one (#1436)?
//
// A private-only instance (--no-public-ip, #1433) reaches S3 through a VPC
// endpoint. A gateway endpoint routes only its own region's S3 addresses, and
// an interface endpoint's private DNS answers only the regional names,
// s3.<region>.amazonaws.com and *.s3.<region>.amazonaws.com. The global
// s3.amazonaws.com (and <bucket>.s3.amazonaws.com) is neither, so a bootstrap
// that fetches spored by it cannot do so in a VPC whose only S3 path is an
// interface endpoint, and the instance runs with no TTL or cost limit.
//
// Two forms are checked, as they appear in the rendered script:
//
//   - every http(s) URL whose host is an S3 name must be the regional
//     virtual-hosted or path-style name of the launch region, or the one
//     cross-region exception every install path documents: the us-east-1
//     fallback bucket by its own regional name; and
//   - every AWS CLI S3 call (`aws s3`, `aws s3api`, the PowerShell `& $aws s3`)
//     must pass --region, and every spored source table entry
//     (`@{ u = 's3://spawn-binaries-<r>/...'; r = '<r>' }`) must name its
//     bucket's own region. AWS CLI v2 sends --region <r> to the regional name,
//     us-east-1 included (botocore's UseGlobalEndpoint is false).
//
// Shell variables holding the instance's region (read from IMDS at boot) are
// resolved to the launch region; a host still holding a `$` after that is
// reported, so a new variable cannot pass unexamined.
package s3hosts

import (
	"fmt"
	"regexp"
	"strings"
)

// FallbackBucket is the bucket every spored install path tries after the
// launch region's, knowingly cross-region (sporedinstall.FallbackRegion).
const FallbackBucket = "spawn-binaries-us-east-1"

// Ref is one S3 location a script reaches.
type Ref struct {
	Line int
	Text string
	// Host is the URL's host after region variables are resolved; "" for a
	// CLI call.
	Host string
}

// Problem is a Ref that breaks the rule, and why.
type Problem struct {
	Ref
	Why string
}

func (p Problem) String() string { return fmt.Sprintf("line %d: %s\n\t%s", p.Line, p.Why, p.Text) }

var (
	urlRe  = regexp.MustCompile("https?://[^\\s\"'`<>|;()]+")
	hostRe = regexp.MustCompile(`^https?://([^/?#]*)`)
	// The region variables the generated scripts read from IMDS.
	regionVarRe = regexp.MustCompile(`\$\{(?:REGION|SPAWN_BS_REGION|region)(?::-[a-z0-9-]+)?\}|\$(?:REGION|SPAWN_BS_REGION|region)\b`)
	// Any S3 name: s3.amazonaws.com, s3.<r>.amazonaws.com, s3-external-1,
	// s3.dualstack.<r>, and each with a bucket in front.
	s3HostRe = regexp.MustCompile(`^(?:.+\.)?s3(?:[.-][a-z0-9.-]+)?\.amazonaws\.com$`)
	// The regional names: <bucket>.s3.<r>.amazonaws.com and s3.<r>.amazonaws.com.
	regionalRe = regexp.MustCompile(`^(?:(.+)\.)?s3\.([a-z]{2}(?:-[a-z]+)+-[0-9])\.amazonaws\.com$`)
	// An AWS CLI S3 call as a command, not prose: aws/spawn_aws/$aws at a
	// command position, then s3 or s3api as the subcommand (global options
	// such as "$AWS_REGION_ARG" may come between).
	cliS3Re     = regexp.MustCompile(`(?:^|[;&|(]|\bthen|\bdo|&&|\|\|)\s*(?:&\s*)?(?:aws|spawn_aws|\$aws)(?:\s+(?:"[^"]*"|'[^']*'|--?[\w-]+(?:=\S+)?))*\s+s3(?:api)?\s`)
	bucketVarRe = regexp.MustCompile(`(?m)^\s*\$bucket = "spawn-binaries-\$region"\s*$`)
	// A PowerShell spored source table entry.
	sourceRe = regexp.MustCompile(`u = (['"])s3://([^/'"]+)/[^'"]*['"];\s*r = ([^ }]+)`)
)

func isComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

// Check returns every S3 location script reaches and every one that breaks
// the rule for a launch in region.
func Check(script, region string) (refs []Ref, problems []Problem) {
	for i, line := range strings.Split(script, "\n") {
		if isComment(line) {
			continue
		}
		n := i + 1
		for _, raw := range urlRe.FindAllString(line, -1) {
			host := hostRe.FindStringSubmatch(regionVarRe.ReplaceAllString(raw, region))[1]
			if strings.Contains(host, "$") {
				if strings.Contains(host, "amazonaws") || strings.Contains(host, "s3") {
					r := Ref{Line: n, Text: raw, Host: host}
					refs = append(refs, r)
					problems = append(problems, Problem{r, "the S3 host holds a variable that is not a known region variable, so its name cannot be checked"})
				}
				continue
			}
			if !s3HostRe.MatchString(host) {
				continue
			}
			r := Ref{Line: n, Text: raw, Host: host}
			refs = append(refs, r)
			m := regionalRe.FindStringSubmatch(host)
			switch {
			case m == nil:
				problems = append(problems, Problem{r, host + " is not a regional S3 name (<bucket>.s3.<region>.amazonaws.com), " +
					"which neither an S3 gateway endpoint nor an interface endpoint's private DNS serves"})
			case m[2] == region:
			case m[1] == FallbackBucket && m[2] == "us-east-1":
				// The documented cross-region fallback, by its own regional name.
			default:
				problems = append(problems, Problem{r, fmt.Sprintf("%s is %s's regional name, not the launch region's (%s)", host, m[2], region)})
			}
		}
		if cliS3Re.MatchString(line) {
			r := Ref{Line: n, Text: strings.TrimSpace(line)}
			refs = append(refs, r)
			if !strings.Contains(line, "--region") && !strings.Contains(line, "$AWS_REGION_ARG") {
				problems = append(problems, Problem{r, "an AWS CLI S3 call without --region"})
			}
		}
		for _, m := range sourceRe.FindAllStringSubmatch(line, -1) {
			r := Ref{Line: n, Text: strings.TrimSpace(m[0])}
			refs = append(refs, r)
			bucket, reg := m[2], strings.Trim(m[3], `'"`)
			switch {
			case bucket == "$bucket" && reg == "$region" && bucketVarRe.MatchString(script):
				// $bucket is the launch region's: $bucket = "spawn-binaries-$region".
			case strings.HasPrefix(bucket, "spawn-binaries-") && strings.TrimPrefix(bucket, "spawn-binaries-") == reg:
			default:
				problems = append(problems, Problem{r, fmt.Sprintf("bucket %s is fetched with region %s, not its own", bucket, m[3])})
			}
		}
	}
	return refs, problems
}

// GlobalHosts returns every http(s) URL in text whose host is an S3 name but
// not a regional one, in any region: the names a source sweep must not find.
func GlobalHosts(text string) []Ref {
	var out []Ref
	for i, line := range strings.Split(text, "\n") {
		for _, raw := range urlRe.FindAllString(line, -1) {
			host := hostRe.FindStringSubmatch(raw)[1]
			if s3HostRe.MatchString(regionVarRe.ReplaceAllString(host, "eu-west-2")) && !regionalRe.MatchString(regionVarRe.ReplaceAllString(host, "eu-west-2")) {
				out = append(out, Ref{Line: i + 1, Text: strings.TrimSpace(line), Host: host})
			}
		}
	}
	return out
}
