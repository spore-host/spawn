package dns

import "testing"

func TestValidateDomain(t *testing.T) {
	for _, ok := range []string{"spore.host", "spore.example.edu", "spore.research.wwps.aws.dev", "a-b.c1"} {
		if err := ValidateDomain(ok); err != nil {
			t.Errorf("ValidateDomain(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "host", "Spore.Host", "-a.b", "a-.b", "a..b", "a.b.", "a b.c", "a_b.c", "x.y/z"} {
		if err := ValidateDomain(bad); err == nil {
			t.Errorf("ValidateDomain(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateEndpoint(t *testing.T) {
	const want = "https://abc123.lambda-url.eu-west-1.on.aws/"
	for _, ok := range []string{
		"https://abc123.lambda-url.eu-west-1.on.aws/",
		"https://abc123.lambda-url.eu-west-1.on.aws",
		"  https://ABC123.lambda-url.eu-west-1.on.aws/ ",
	} {
		got, err := ValidateEndpoint(ok)
		if err != nil || got != want {
			t.Errorf("ValidateEndpoint(%q) = (%q, %v), want (%q, nil)", ok, got, err, want)
		}
	}
	for _, bad := range []string{
		"",
		"http://abc123.lambda-url.eu-west-1.on.aws/",                             // not https
		"https://abc.execute-api.us-east-1.amazonaws.com/prod/x",                 // API Gateway: cannot be signed for lambda
		"https://dns.example.edu/",                                               // custom domain
		"https://abc123.lambda-url.eu-west-1.on.aws.evil.example/",               // suffix trick
		"https://user:pw@abc123.lambda-url.eu-west-1.on.aws/",                    // userinfo
		"https://abc123.lambda-url.eu-west-1.on.aws/update",                      // path
		"https://abc123.lambda-url.eu-west-1.on.aws/?x=1",                        // query
		"https://abc123.lambda-url.eu-west-1.on.aws:8443/",                       // port
		"https://f4gm19tl70.execute-api.us-east-1.amazonaws.com/prod/update-dns", // the deleted #872 default
	} {
		if got, err := ValidateEndpoint(bad); err == nil {
			t.Errorf("ValidateEndpoint(%q) = %q, want an error", bad, got)
		}
	}
}

// The region a request is signed for must come from the validated endpoint, so
// a custom updater in another region is signed for THAT region.
func TestAValidatedEndpointSignsForItsOwnRegion(t *testing.T) {
	ep, err := ValidateEndpoint("https://abc123.lambda-url.ap-southeast-2.on.aws/")
	if err != nil {
		t.Fatal(err)
	}
	if got := signingRegion(ep, "us-east-1"); got != "ap-southeast-2" {
		t.Errorf("signingRegion = %q, want ap-southeast-2", got)
	}
}
