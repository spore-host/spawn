package dns

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DefaultDomain is the zone a spore registers under when nothing names another.
const DefaultDomain = defaultDomain

// The custom DNS target (#891): a domain and an updater endpoint other than
// spore.host's. Both travel to the instance as tags (spawn:dns-domain,
// spawn:dns-endpoint) and are checked twice — by the CLI before anything is
// launched, and by spored before it uses them, since a tag is not a trusted input.

// domainLabel is one lowercase DNS label: letters, digits and inner hyphens.
var domainLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateDomain reports whether d can be a registration zone: at least two
// labels, each a valid lowercase DNS label, 253 characters at most. It does not
// check that the updater serves the zone — only the updater knows that, and it
// answers "Unknown domain" for one it does not.
func ValidateDomain(d string) error {
	if d == "" || len(d) > 253 || d != strings.ToLower(d) {
		return fmt.Errorf("invalid DNS domain %q: must be a lowercase domain name", d)
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return fmt.Errorf("invalid DNS domain %q: needs at least two labels (e.g. spore.example.edu)", d)
	}
	for _, l := range labels {
		if !domainLabel.MatchString(l) {
			return fmt.Errorf("invalid DNS domain %q: label %q is not a valid DNS label", d, l)
		}
	}
	return nil
}

// ValidateEndpoint reports whether u can be a DNS updater endpoint, and returns
// it in canonical form.
//
// Only an https Lambda Function URL is accepted. That is not a style rule:
// requests are SigV4-signed for the "lambda" service in the region the hostname
// names (signingRegion), and the updater authorizes the account that signature
// verifies (#173). An API Gateway URL, a custom domain or a plain-http endpoint
// cannot be signed for correctly, so it could only ever fail with a 403 — or,
// for an unauthenticated endpoint, succeed without anyone being verified.
// Userinfo, a query or a path beyond "/" are refused for the same reason: none
// of them is part of a Function URL.
func ValidateEndpoint(u string) (string, error) {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return "", fmt.Errorf("invalid DNS endpoint %q: %w", u, err)
	}
	if p.Scheme != "https" {
		return "", fmt.Errorf("invalid DNS endpoint %q: must be https", u)
	}
	if p.User != nil || p.RawQuery != "" || p.Fragment != "" || (p.Path != "" && p.Path != "/") || p.Port() != "" {
		return "", fmt.Errorf("invalid DNS endpoint %q: must be a bare Lambda Function URL (https://<id>.lambda-url.<region>.on.aws/)", u)
	}
	host := strings.ToLower(p.Hostname())
	if !functionURLHost.MatchString(host) {
		return "", fmt.Errorf("invalid DNS endpoint %q: must be a Lambda Function URL (https://<id>.lambda-url.<region>.on.aws/) — "+
			"requests are SigV4-signed for Lambda, so no other kind of endpoint can authenticate", u)
	}
	return "https://" + host + "/", nil
}

// functionARN is a Lambda function ARN without a qualifier: partition, region,
// a 12-digit account and the function name. No wildcards and no policy
// variables — it becomes the Resource of the instance role's
// lambda:InvokeFunctionUrl grant (#1158), and IAM rejects a policy variable there
// (#1069).
var functionARN = regexp.MustCompile(`^arn:aws(-[a-z]+)*:lambda:([a-z0-9-]+):([0-9]{12}):function:([A-Za-z0-9_-]{1,64})$`)

// ValidateFunctionARN reports whether arn can name the Lambda function behind
// endpoint (already validated by ValidateEndpoint): a plain function ARN in the
// region the endpoint's hostname names, since a Function URL belongs to a
// function in its own region.
func ValidateFunctionARN(arn, endpoint string) error {
	m := functionARN.FindStringSubmatch(strings.TrimSpace(arn))
	if m == nil {
		return fmt.Errorf("invalid DNS updater function ARN %q: must be arn:aws:lambda:<region>:<account>:function:<name>", arn)
	}
	if want := signingRegion(endpoint, ""); m[2] != want {
		return fmt.Errorf("DNS updater function ARN %q is in %s, but the endpoint %s is in %s: a Function URL belongs to a function in its own region", arn, m[2], endpoint, want)
	}
	return nil
}
