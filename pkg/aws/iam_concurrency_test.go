package aws

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestCreateOrGetInstanceProfile_Concurrent verifies that many concurrent
// launches ensuring the SAME shared role/profile all succeed, rather than
// racing on check-then-act and failing with EntityAlreadyExists (#64).
func TestCreateOrGetInstanceProfile_Concurrent(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	const n = 8
	cfg := IAMRoleConfig{
		RoleName:      "spawn-instance-concurrent-test",
		TrustServices: []string{"ec2.amazonaws.com"},
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	profiles := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			profiles[i], errs[i] = c.CreateOrGetInstanceProfile(context.Background(), cfg)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent CreateOrGetInstanceProfile[%d] failed: %v", i, err)
		}
		if profiles[i] != cfg.RoleName {
			t.Errorf("profile[%d] = %q, want %q", i, profiles[i], cfg.RoleName)
		}
	}
}

// TestIsAlreadyExists covers the benign-error classifier used by retryIAM.
func TestIsAlreadyExists(t *testing.T) {
	if !isAlreadyExists(&types.EntityAlreadyExistsException{}) {
		t.Error("EntityAlreadyExistsException should be treated as already-exists")
	}
	if !isAlreadyExists(&types.LimitExceededException{}) {
		t.Error("LimitExceededException (already-attached role) should be treated as already-exists")
	}
	if isAlreadyExists(nil) {
		t.Error("nil must not be already-exists")
	}
	if isAlreadyExists(errors.New("some other error")) {
		t.Error("unrelated error must not be already-exists")
	}
}

// TestRetryIAM_AlreadyExistsIsSuccess verifies retryIAM treats already-exists as
// success and returns a genuine error unchanged.
func TestRetryIAM_AlreadyExistsIsSuccess(t *testing.T) {
	if err := retryIAM(func() error { return &types.EntityAlreadyExistsException{} }); err != nil {
		t.Errorf("already-exists should be success, got %v", err)
	}
	sentinel := errors.New("boom")
	if err := retryIAM(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("non-retryable error should pass through, got %v", err)
	}
}
