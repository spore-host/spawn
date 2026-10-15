package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Identity.AWSRegion is the guard against pinning an AWS client to the local
// provider's "local" placeholder (#899). Tested on the struct directly, not only
// through NewLocalProvider, so a caller building an Identity by hand is covered.
func TestIdentityAWSRegion(t *testing.T) {
	for _, tc := range []struct {
		id   *Identity
		want string
	}{
		{&Identity{Region: "us-west-2"}, "us-west-2"},
		{&Identity{Region: "local"}, ""},
		{&Identity{Region: ""}, ""},
		{nil, ""},
	} {
		if got := tc.id.AWSRegion(); got != tc.want {
			t.Errorf("%+v.AWSRegion() = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// The local provider's DynamoDB client reads the same hybrid-registry table that
// pkg/registry writes, so it must be pinned to the same region — the configured
// one — rather than whatever the ambient chain resolves (#899).
func TestNewLocalProvider_PinsTheRegistryClientToTheConfiguredRegion(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
	t.Setenv("SPAWN_REGION", "")

	for _, tc := range []struct{ region, want string }{
		{"us-west-2", "us-west-2"},
		{"local", "eu-west-1"}, // the placeholder is not a region: ambient decides
	} {
		t.Run(tc.region, func(t *testing.T) {
			path := filepath.Join(dir, tc.region+".yaml")
			if err := os.WriteFile(path, []byte("instance_id: test-local-01\nregion: "+tc.region+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SPAWN_CONFIG", path)
			p, err := NewLocalProvider(context.Background())
			if err != nil {
				t.Fatalf("NewLocalProvider: %v", err)
			}
			if p.dynamoClient == nil {
				t.Fatal("dynamoClient not initialized")
			}
			if got := p.dynamoClient.Options().Region; got != tc.want {
				t.Fatalf("local provider DynamoDB region = %q, want %q", got, tc.want)
			}
		})
	}
}
