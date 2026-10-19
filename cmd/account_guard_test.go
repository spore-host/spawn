package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// stubAccount makes the account guard see account for every credential set
// and counts the lookups.
func stubAccount(t *testing.T, account string) *int {
	t.Helper()
	n := 0
	t.Cleanup(spawnconfig.SetCallerAccountForTest(func(context.Context, sdkaws.Config) (string, error) {
		n++
		return account, nil
	}))
	return &n
}

// TestAccountGuardRefusesBeforeAnyAWSCall is #1118: `spawn --account X burst`
// with credentials for another account launched anyway. burst is used because
// TestBurstHonoursTheRegionFlag shows that, through this same recorder, it
// reaches RunInstances; with the guard refusing, nothing at all may reach AWS.
func TestAccountGuardRefusesBeforeAnyAWSCall(t *testing.T) {
	for name, pin := range map[string]func(t *testing.T) []string{
		"--account": func(t *testing.T) []string { return []string{"--account", "111111111111"} },
		"SPORE_ACCOUNT": func(t *testing.T) []string {
			t.Setenv("SPORE_ACCOUNT", "111111111111")
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			isolateAWSEnv(t)
			t.Setenv("AWS_REGION", "us-west-2")
			rec := recordAWS(t)
			lookups := stubAccount(t, "222222222222")

			err := runRootForTest(t, append(pin(t), burstArgs()...)...)
			var mm *spawnconfig.AccountMismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("burst with a mismatched account = %v, want an AccountMismatchError", err)
			}
			if mm.Want != "111111111111" || mm.Got != "222222222222" || mm.Source != name {
				t.Errorf("refusal = %+v, want want=111111111111 got=222222222222 source=%s", *mm, name)
			}
			if *lookups == 0 {
				t.Error("the guard never looked the account up")
			}
			if calls := rec.snapshot(); len(calls) != 0 {
				t.Errorf("burst sent %d AWS request(s) after the guard should have refused: %+v", len(calls), calls)
			}
		})
	}
}

// TestAccountGuardLetsAMatchThrough: the guard must not block the account the
// user asked for. Same command, matching account, reaches RunInstances.
func TestAccountGuardLetsAMatchThrough(t *testing.T) {
	isolateAWSEnv(t)
	t.Setenv("AWS_REGION", "us-west-2")
	rec := recordAWS(t)
	stubAccount(t, "111111111111")

	err := runRootForTest(t, append([]string{"--account", "111111111111"}, burstArgs()...)...)
	var mm *spawnconfig.AccountMismatchError
	if errors.As(err, &mm) {
		t.Fatalf("burst refused a matching account: %v", err)
	}
	assertBurstRegion(t, rec, "us-west-2")
}

// TestOfflineCommandsDoNotCallSTS: the guard is paid for by building an AWS
// config, so a command that needs no AWS never asks STS, even with an account
// pinned (otherwise SPORE_ACCOUNT in a profile would make `spawn version` need
// the network).
func TestOfflineCommandsDoNotCallSTS(t *testing.T) {
	for _, args := range [][]string{
		{"version"},
		{"completion", "bash"},
		{"help"},
		// Not `launch --help` / `help launch`: cobra then adds a --help flag to
		// launchCmd's own flag set, and TestEveryLaunchFlagHasAWindowsRule,
		// which enumerates that set, would see it.
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateAWSEnv(t)
			lookups := stubAccount(t, "222222222222")
			if err := runRootForTest(t, append([]string{"--account", "111111111111"}, args...)...); err != nil {
				t.Fatalf("spawn %v with --account: %v", args, err)
			}
			if *lookups != 0 {
				t.Errorf("spawn %v made %d STS lookup(s); an offline command must make none", args, *lookups)
			}
		})
	}
}
