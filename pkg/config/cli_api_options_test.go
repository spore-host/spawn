package config

import (
	"context"
	"reflect"
	"testing"

	"github.com/aws/smithy-go/middleware"
)

// marker is a recognisable API option.
func marker(*middleware.Stack) error { return nil }

func hasMarker(opts []func(*middleware.Stack) error) bool {
	want := reflect.ValueOf(marker).Pointer()
	for _, o := range opts {
		if reflect.ValueOf(o).Pointer() == want {
			return true
		}
	}
	return false
}

func setMarker(t *testing.T) {
	t.Helper()
	SetCLIAPIOptions(marker)
	t.Cleanup(func() { SetCLIAPIOptions() })
}

// TestCLIAPIOptions: unset in any program but the spawn CLI (nil), set by
// SetCLIAPIOptions, and handed out as a copy.
func TestCLIAPIOptions(t *testing.T) {
	if got := CLIAPIOptions(); got != nil {
		t.Fatalf("CLIAPIOptions before any set = %d option(s), want none", len(got))
	}
	setMarker(t)
	got := CLIAPIOptions()
	if len(got) != 1 || !hasMarker(got) {
		t.Fatalf("CLIAPIOptions = %d option(s)", len(got))
	}
	got[0] = nil
	if !hasMarker(CLIAPIOptions()) {
		t.Error("changing the returned slice changed the set")
	}
	SetCLIAPIOptions()
	if CLIAPIOptions() != nil {
		t.Error("SetCLIAPIOptions() did not clear the set")
	}
}

// TestLoadersAddTheCLIAPIOptions: the compute loader always adds them; the
// infra loader only when infra is the caller's own profile, never for a
// dedicated infra profile (another account).
func TestLoadersAddTheCLIAPIOptions(t *testing.T) {
	isolateGuardEnv(t)
	stubCaller(t, "123456789012", nil)
	ctx := context.Background()

	cfg, err := LoadComputeAWSConfig(ctx, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if hasMarker(cfg.APIOptions) {
		t.Fatal("an option before any set")
	}
	setMarker(t)
	if cfg, err = LoadComputeAWSConfig(ctx, "us-east-1"); err != nil || !hasMarker(cfg.APIOptions) {
		t.Errorf("compute config lacks the CLI's options (%v)", err)
	}

	// The default infra profile, spore-host-infra, is another account's.
	if cfg, err = LoadInfraAWSConfig(ctx, "us-east-1"); err != nil {
		t.Fatal(err)
	} else if hasMarker(cfg.APIOptions) {
		t.Error("a dedicated infra profile's config carries the CLI's account note")
	}
	// SPAWN_INFRA_PROFILE="" falls to the caller's own profile.
	t.Setenv("SPAWN_INFRA_PROFILE", "")
	if cfg, err = LoadInfraAWSConfig(ctx, "us-east-1"); err != nil || !hasMarker(cfg.APIOptions) {
		t.Errorf("infra as the caller's own profile lacks the CLI's options (%v)", err)
	}
}
