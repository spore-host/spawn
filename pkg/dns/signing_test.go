package dns

import (
	"context"
	"testing"
)

// A client built with NO environment must sign (#883). That is exactly what the
// Windows spored service gets — it sets no variables — and an unsigned request to
// the AWS_IAM Function URL is refused with 403 regardless of resource policy.
func TestClientSignsWithNoEnvironment(t *testing.T) {
	t.Setenv("SPORE_DNS_SIGV4", "")
	c, err := NewClient(context.Background(), "spore.host", "")
	if err != nil {
		t.Fatal(err)
	}
	if !c.sign {
		t.Fatal("a client with SPORE_DNS_SIGV4 unset does not sign — every Windows spore's " +
			"DNS registration is refused with 403 (#883)")
	}
	if c.signer == nil || c.credsProvider == nil {
		t.Error("signing is on but the signer or credentials were not wired")
	}
}

func TestSigningCanBeExplicitlyDisabled(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE", "off", "no", " no "} {
		t.Setenv("SPORE_DNS_SIGV4", v)
		if sigV4Enabled() {
			t.Errorf("SPORE_DNS_SIGV4=%q did not opt out", v)
		}
	}
	// Anything else — including the "1" the Linux units still set — signs.
	for _, v := range []string{"", "1", "true", "yes"} {
		t.Setenv("SPORE_DNS_SIGV4", v)
		if !sigV4Enabled() {
			t.Errorf("SPORE_DNS_SIGV4=%q disabled signing", v)
		}
	}
}

// The signature must be scoped to the Function URL's region, not the client's.
func TestSigningRegionFollowsTheEndpoint(t *testing.T) {
	const prod = "https://zqonqra6blwh7342ujuxv3bwei0wnpyq.lambda-url.us-east-1.on.aws/"
	cases := []struct {
		endpoint, fallback, want string
	}{
		{prod, "", "us-east-1"},
		// THE accident this removes: an ambient region outside us-east-1 must not
		// override the endpoint's own region, or the signature is refused.
		{prod, "eu-west-2", "us-east-1"},
		{"https://abc123.lambda-url.eu-west-1.on.aws/", "us-east-1", "eu-west-1"},
		// A custom endpoint carries no region: use the ambient one, else us-east-1.
		{"https://dns.example.org/update", "ap-south-1", "ap-south-1"},
		{"https://dns.example.org/update", "", "us-east-1"},
		{"not a url", "", "us-east-1"},
	}
	for _, c := range cases {
		if got := signingRegion(c.endpoint, c.fallback); got != c.want {
			t.Errorf("signingRegion(%q, %q) = %q, want %q", c.endpoint, c.fallback, got, c.want)
		}
	}
}

// End to end through NewClient: an ambient AWS_REGION outside the Function URL's
// region must not change the region the client signs for.
func TestClientSignsForTheEndpointRegionDespiteAWSRegion(t *testing.T) {
	t.Setenv("SPORE_DNS_SIGV4", "")
	t.Setenv("AWS_REGION", "eu-west-2")
	c, err := NewClient(context.Background(), "spore.host",
		"https://zqonqra6blwh7342ujuxv3bwei0wnpyq.lambda-url.us-east-1.on.aws/")
	if err != nil {
		t.Fatal(err)
	}
	if c.region != "us-east-1" {
		t.Errorf("client signs for %q with AWS_REGION=eu-west-2; the Function URL is in us-east-1, "+
			"so the signature would be refused", c.region)
	}
}
