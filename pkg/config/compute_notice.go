package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// The compute-override notice (#1538). SPAWN_COMPUTE_PROFILE and
// compute_profile exist for split setups (the spore.host team launches in a
// dedicated dev account), so a compute account other than the caller's is not
// refused. It is never silent either: before #1538 a hardcoded override sent a
// detached sweep's members to another account than the one AWS_PROFILE named,
// and nothing said so.

// ComputeNoticeOutput is where the notice goes. A variable for tests.
var ComputeNoticeOutput io.Writer = os.Stderr

var (
	computeNoticeMu   sync.Mutex
	computeNoticeDone = map[string]bool{} // override profile → printed
)

// ResetComputeNoticeForTest forgets which notices were printed.
func ResetComputeNoticeForTest() {
	computeNoticeMu.Lock()
	defer computeNoticeMu.Unlock()
	computeNoticeDone = map[string]bool{}
}

// noteComputeOverride prints, once per process and profile, a notice naming
// the compute account when an override chose a profile other than the
// caller's own and the two resolve to different accounts, or when either
// account cannot be determined. No override, or an override naming the
// caller's own profile, costs nothing: compute is then the caller's account.
func noteComputeOverride(ctx context.Context, computeCfg aws.Config, cp ComputeProfile) {
	if !cp.Override || cp.Name == SharedProfile() {
		return
	}
	computeNoticeMu.Lock()
	done := computeNoticeDone[cp.Name]
	computeNoticeDone[cp.Name] = true
	computeNoticeMu.Unlock()
	if done {
		return
	}
	if msg := computeOverrideNotice(ctx, computeCfg, cp); msg != "" {
		_, _ = fmt.Fprintln(ComputeNoticeOutput, msg)
	}
}

// computeOverrideNotice is the notice's text, or "" when the override and the
// caller's own credentials are the same account.
func computeOverrideNotice(ctx context.Context, computeCfg aws.Config, cp ComputeProfile) string {
	ctx, cancel := context.WithTimeout(ctx, accountGuardTimeout)
	defer cancel()
	compute, cerr := callerAccount(ctx, computeCfg)
	ambient, aerr := ambientAccount(ctx)
	own := callerComputeProfile()
	if cerr == nil && aerr == nil && compute != "" && compute == ambient {
		return ""
	}
	show := func(acct string, err error) string {
		if err != nil || acct == "" {
			return "an account that could not be determined"
		}
		return "account " + acct
	}
	return fmt.Sprintf("⚠️  Compute operations use %s (%s), not your own credentials' %s (%s). "+
		"Instances launch there. Unset %s to use your own account.",
		show(compute, cerr), cp.Describe(), show(ambient, aerr), own.Describe(), cp.Source)
}

// ambientAccount is the account of the caller's own credentials (the shared
// profile, else the ambient chain). A variable so tests can stand in for STS.
var ambientAccount = loadAmbientAccount

func loadAmbientAccount(ctx context.Context) (string, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if p := SharedProfile(); p != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(p))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return "", err
	}
	// The guard is for configs handed out; this one is only asked its
	// account, and a mismatch with a pinned --account already carries it.
	if err := CheckAccount(ctx, cfg); err != nil {
		var mm *AccountMismatchError
		if errors.As(err, &mm) {
			return mm.Got, nil
		}
		return "", err
	}
	return callerAccount(ctx, cfg)
}

// SetAmbientAccountForTest replaces the caller's-own-account lookup the
// compute notice uses and returns a function restoring it.
func SetAmbientAccountForTest(f func(context.Context) (string, error)) (restore func()) {
	prev := ambientAccount
	ambientAccount = f
	return func() { ambientAccount = prev }
}
