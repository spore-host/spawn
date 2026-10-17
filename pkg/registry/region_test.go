package registry

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/provider"
)

// onAnInstance strips every ambient source of a region, leaving the environment
// spored actually runs in: no AWS_REGION, no AWS_DEFAULT_REGION and no shared
// config naming one (#899).
func onAnInstance(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
}

// The hybrid-registry client must carry a region on an instance, where nothing
// ambient supplies one. It used to load the ambient chain alone and every
// Register failed with "Missing Region" before reaching DynamoDB (#899). The
// region is the instance's own: `spawn burst` creates the table in the region
// it launches the burst instances into.
func TestNewPeerRegistry_UsesTheInstanceRegionWhenNothingAmbientSetsOne(t *testing.T) {
	onAnInstance(t)
	reg, err := NewPeerRegistry(context.Background(), &provider.Identity{
		InstanceID: "i-0123456789abcdef0", Region: "us-west-2", Provider: "ec2",
	})
	if err != nil {
		t.Fatalf("NewPeerRegistry: %v", err)
	}
	if got := reg.client.Options().Region; got != "us-west-2" {
		t.Fatalf("registry client region = %q, want the instance's region us-west-2", got)
	}
}

// The instance's region beats an ambient one, matching every other spored AWS
// client (agent.go pins identity.Region): the table is where the instance is.
func TestNewPeerRegistry_InstanceRegionBeatsAmbient(t *testing.T) {
	onAnInstance(t)
	t.Setenv("AWS_REGION", "eu-west-1")
	reg, err := NewPeerRegistry(context.Background(), &provider.Identity{Region: "us-west-2", Provider: "ec2"})
	if err != nil {
		t.Fatalf("NewPeerRegistry: %v", err)
	}
	if got := reg.client.Options().Region; got != "us-west-2" {
		t.Fatalf("registry client region = %q, want us-west-2", got)
	}
}

// A local provider with no configured region reports the placeholder "local".
// That is not a region; pinning to it would send DynamoDB to
// dynamodb.local.amazonaws.com. The ambient chain must still decide.
func TestNewPeerRegistry_LocalPlaceholderFallsBackToAmbient(t *testing.T) {
	for _, region := range []string{"", "local"} {
		t.Run("region="+region, func(t *testing.T) {
			onAnInstance(t)
			t.Setenv("AWS_REGION", "eu-west-1")
			reg, err := NewPeerRegistry(context.Background(), &provider.Identity{Region: region, Provider: "local"})
			if err != nil {
				t.Fatalf("NewPeerRegistry: %v", err)
			}
			if got := reg.client.Options().Region; got != "eu-west-1" {
				t.Fatalf("registry client region = %q, want the ambient eu-west-1", got)
			}
		})
	}
}

// noAmbientRegion clears every source the SDK's default chain reads a region
// from, as on an EC2 instance whose spored unit sets no AWS_REGION. It also
// removes every credential source, so a regression that bypasses the test's
// recorder cannot reach a real account.
func noAmbientRegion(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
}

// hostRecorder is an HTTP client that never reaches the network: it records the
// host of every request and fails it before anything is sent.
type hostRecorder struct {
	mu    sync.Mutex
	hosts []string
}

func (h *hostRecorder) Do(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.hosts = append(h.hosts, req.URL.Host)
	h.mu.Unlock()
	return nil, errors.New("hostRecorder: no network in tests")
}

// TestEnsureTableUsesTheCallersRegion is #939: EnsureTable loaded the ambient
// default config, so `spawn burst --region us-west-2` created (or found) the
// registry table wherever the ambient chain pointed. The table must be created
// in the region of the config the caller passes, which is the region the
// members launch into.
func TestEnsureTableUsesTheCallersRegion(t *testing.T) {
	noAmbientRegion(t)
	t.Setenv("AWS_REGION", "eu-west-1") // ambient says one thing...

	rec := &hostRecorder{}
	cfg := aws.Config{
		Region:      "us-west-2", // ...the caller says another
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  rec,
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
	_ = EnsureTable(context.Background(), cfg) // fails at the recorder; only the host matters

	if len(rec.hosts) == 0 {
		t.Fatal("EnsureTable made no DynamoDB request")
	}
	for _, h := range rec.hosts {
		if !strings.Contains(h, "us-west-2") {
			t.Errorf("EnsureTable sent a request to %s, want the caller's region us-west-2", h)
		}
	}
}

// TestEnsureTableRefusesAConfigWithNoRegion: with no region there is no table
// a member could be pointed at, so fail with that rather than an SDK endpoint
// error.
func TestEnsureTableRefusesAConfigWithNoRegion(t *testing.T) {
	err := EnsureTable(context.Background(), aws.Config{})
	if err == nil || !strings.Contains(err.Error(), "no AWS region") {
		t.Fatalf("EnsureTable(no region) = %v, want a no-region error", err)
	}
}
