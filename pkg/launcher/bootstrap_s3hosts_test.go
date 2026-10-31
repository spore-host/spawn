package launcher

import (
	"testing"

	"github.com/spore-host/spawn/internal/s3hosts"
)

// TestEveryBootstrapS3HostIsRegional is the gate for #1436: every S3 name the
// Linux bootstrap reaches is the launch region's regional one (or the
// us-east-1 fallback bucket by its own regional name), so a private-only
// instance (--no-public-ip, #1433) is served by an S3 gateway endpoint or an
// interface endpoint with private DNS, which the pre-flight in
// pkg/aws/private_network.go counts. Rendered as each ships: the stub a release
// build puts in user data, the inline bootstrap of a dev build, and the
// published body, for a region other than us-east-1, where a global name and
// the regional one differ.
func TestEveryBootstrapS3HostIsRegional(t *testing.T) {
	const region = "eu-west-2"
	cfg := BootstrapConfig{Username: "ec2-user", Command: "echo hi", TaskFlushScript: "#!/bin/bash\necho flush\n"}
	stub, err := buildLinuxBootstrap(cfg, testRelease, testRelease, BootstrapBody())
	if err != nil {
		t.Fatal(err)
	}
	inline, err := buildLinuxBootstrap(cfg, "", "", BootstrapBody())
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"release stub (user data)":         stub,
		"inline bootstrap (dev user data)": inline,
		"published body":                   BootstrapBody(),
	} {
		refs, problems := s3hosts.Check(script, region)
		// The stub fetches the body, and the body (inline or published) spored:
		// each has at least the regional source and the fallback.
		if len(refs) < 2 {
			t.Errorf("%s: found %d S3 locations, want at least the regional bucket and the fallback; discovery has broken", name, len(refs))
		}
		for _, p := range problems {
			t.Errorf("%s: %s", name, p)
		}
	}
}
