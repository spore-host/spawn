package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The account guard (#1118). `--account`, SPORE_ACCOUNT and `account =` in the
// shared spore config name the AWS account the user expects to act in. Before
// #1118 spawn recorded the value and only `spawn doctor` ever read it, so
// `spawn --account 111111111111 launch` launched into whatever account the
// credentials resolved to. People set the guard precisely because spawn creates
// billable resources, so a guard that silently does nothing is worse than none.
//
// The check lives at the credential roots rather than in a cobra hook: every
// AWS config spawn builds from a credential chain calls CheckAccount on it
// before handing it out (spawnaws.NewClientWithRegion, LoadComputeAWSConfig,
// LoadInfraAWSConfig when it resolves the user's own profile, and the other
// load sites linked into the CLI). A command that never builds an AWS config
// (version, completion, help, docs, local config) therefore never calls STS,
// and a command that does cannot reach a mutating call without passing the
// guard first, because it has no config to make the call with.
// account_guard_coverage_test.go at the module root discovers every
// LoadDefaultConfig call in the packages linked into the spawn binary and fails
// if one does not go through CheckAccount.
//
// Semantics match truffle#208 / lagotto#180: with no expected account there is
// no STS call; with one, a mismatch, an STS error or an empty account is a
// refusal. Unlike those, spawn also enforces SPORE_ACCOUNT and the config file,
// which spawn doctor already reported on.

// accountGuardTimeout bounds the one STS call the guard costs per credential set.
const accountGuardTimeout = 30 * time.Second

// AccountMismatchError is CheckAccount's refusal when the credentials belong to
// another account. Source names where the expected account came from.
type AccountMismatchError struct {
	Source string // "--account", "SPORE_ACCOUNT" or the config file
	Want   string
	Got    string
}

func (e *AccountMismatchError) Error() string {
	return fmt.Sprintf("%s %s: the current AWS credentials belong to account %s; refusing to continue (check --profile / SPORE_PROFILE / AWS_PROFILE)", e.Source, e.Want, e.Got)
}

// ExpectedAccount returns the account the user pinned and where it came from,
// with the shared config's precedence (flag > env > file). It returns "", "" when
// no account is pinned.
func ExpectedAccount() (account, source string) {
	account = strings.TrimSpace(SharedConfig().Account)
	if account == "" {
		return "", ""
	}
	sharedMu.RLock()
	flag := strings.TrimSpace(sharedFlags.Account)
	sharedMu.RUnlock()
	switch {
	case flag != "":
		return account, "--account"
	case strings.TrimSpace(os.Getenv("SPORE_ACCOUNT")) != "":
		return account, "SPORE_ACCOUNT"
	default:
		return account, "account (spore config file)"
	}
}

// CheckAccount refuses cfg when the user pinned an expected account and cfg's
// credentials resolve to a different one, or to none that STS can name. With no
// account pinned it returns nil without any AWS call.
func CheckAccount(ctx context.Context, cfg aws.Config) error {
	want, source := ExpectedAccount()
	if want == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, accountGuardTimeout)
	defer cancel()
	got, err := callerAccount(ctx, cfg)
	if err == nil && got == "" {
		err = errors.New("STS returned no account")
	}
	if err != nil {
		return fmt.Errorf("%s %s: could not determine the AWS account of the current credentials: %w", source, want, err)
	}
	if got != want {
		return &AccountMismatchError{Source: source, Want: want, Got: got}
	}
	return nil
}

// callerAccount is how CheckAccount learns the account of a config; a variable
// so tests can stand in for STS.
var callerAccount = stsCallerAccount

// SetCallerAccountForTest replaces the STS lookup CheckAccount uses and returns
// a function restoring the real one. For tests in other packages only.
func SetCallerAccountForTest(f func(context.Context, aws.Config) (string, error)) (restore func()) {
	prev := callerAccount
	callerAccount = f
	return func() { callerAccount = prev }
}

var (
	callerAccountMu    sync.Mutex
	callerAccountCache = map[string]string{} // access key ID → account
)

// stsCallerAccount returns the account of cfg's credentials via STS
// GetCallerIdentity, which is read-only and needs no IAM permission. It is
// cached per access key, so a command building several clients from the same
// credentials pays for one call.
func stsCallerAccount(ctx context.Context, cfg aws.Config) (string, error) {
	if cfg.Credentials == nil {
		return "", errors.New("no credentials")
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", err
	}
	callerAccountMu.Lock()
	cached, ok := callerAccountCache[creds.AccessKeyID]
	callerAccountMu.Unlock()
	if ok {
		return cached, nil
	}

	stsCfg := cfg.Copy()
	if stsCfg.Region == "" {
		stsCfg.Region = "us-east-1" // STS is global; it needs some region to sign for
	}
	out, err := sts.NewFromConfig(stsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	account := aws.ToString(out.Account)
	if account != "" {
		callerAccountMu.Lock()
		callerAccountCache[creds.AccessKeyID] = account
		callerAccountMu.Unlock()
	}
	return account, nil
}
