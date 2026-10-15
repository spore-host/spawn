package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// fakeIMDS serves the IMDSv2 token and the identity document the SDK's region
// lookup reads.
func fakeIMDS(t *testing.T, region string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
			_, _ = w.Write([]byte("token"))
		case r.Method == http.MethodGet && r.URL.Path == "/latest/dynamic/instance-identity/document":
			_, _ = w.Write([]byte(`{"region":"` + region + `","instanceId":"i-0123456789abcdef0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", srv.URL)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "")
}

func noAmbientRegion(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
}

// `spored run-queue` runs on an instance with no AWS_REGION, and the SDK does not
// read the region from IMDS unless asked, so the results uploader failed every
// PutObject with "Missing Region" (#899). The runner's config must take the
// instance's region from IMDS.
func TestQueueRunnerAWSConfig_TakesTheInstanceRegionFromIMDS(t *testing.T) {
	noAmbientRegion(t)
	fakeIMDS(t, "us-west-2")
	cfg, err := loadQueueRunnerAWSConfig(context.Background())
	if err != nil {
		t.Fatalf("loadQueueRunnerAWSConfig: %v", err)
	}
	if cfg.Region != "us-west-2" {
		t.Fatalf("queue runner region = %q, want the instance's us-west-2 from IMDS", cfg.Region)
	}
}

// An explicit region still wins over IMDS.
func TestQueueRunnerAWSConfig_AmbientRegionBeatsIMDS(t *testing.T) {
	noAmbientRegion(t)
	fakeIMDS(t, "us-west-2")
	t.Setenv("AWS_REGION", "eu-west-1")
	cfg, err := loadQueueRunnerAWSConfig(context.Background())
	if err != nil {
		t.Fatalf("loadQueueRunnerAWSConfig: %v", err)
	}
	if cfg.Region != "eu-west-1" {
		t.Fatalf("queue runner region = %q, want the explicit eu-west-1", cfg.Region)
	}
}
