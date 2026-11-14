package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

// fakeSTS records the roles it was asked to assume.
type fakeSTS struct{ assumed []string }

func (f *fakeSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.assumed = append(f.assumed, aws.ToString(in.RoleArn))
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{
		AccessKeyId: aws.String("AK"), SecretAccessKey: aws.String("SK"), SessionToken: aws.String("ST"),
	}}, nil
}

// The record's aws_account_id is writer-chosen (#1584). Whoever wrote it, the
// orchestrator never asks STS for a role in an account no sweep may act in.
func TestAssumeSweepRoleRefusesForbiddenTargets(t *testing.T) {
	prev := crossAccountRole
	crossAccountRole = defaultCrossAccountRole
	t.Cleanup(func() { crossAccountRole = prev })

	own := "966362334030"
	ctx := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{
		InvokedFunctionArn: "arn:aws:lambda:us-east-1:" + own + ":function:spawn-sweep-orchestrator",
	})
	for _, bad := range []string{
		"942542972736", // forbidden (pkg/trustboundary)
		own,            // the orchestrator's own control-plane account
		"",
		"*",
		"43541598422",
		"4354159842260",
		"435415984226:role/x",
		"435415984226/../",
	} {
		f := &fakeSTS{}
		if _, err := assumeSweepRoleWith(ctx, f, bad); err == nil {
			t.Errorf("account %q: assumed; want a refusal", bad)
		}
		if len(f.assumed) != 0 {
			t.Errorf("account %q: STS was asked for %v before the refusal", bad, f.assumed)
		}
	}

	f := &fakeSTS{}
	creds, err := assumeSweepRoleWith(ctx, f, "435415984226")
	if err != nil || creds == nil {
		t.Fatalf("a member account was refused: %v", err)
	}
	if want := "arn:aws:iam::435415984226:role/SpawnSweepCrossAccountRole"; len(f.assumed) != 1 || f.assumed[0] != want {
		t.Errorf("assumed %v, want [%s]", f.assumed, want)
	}
}

func TestCheckSweepTargetWithoutLambdaContext(t *testing.T) {
	if ownAccount(context.Background()) != "" {
		t.Fatal("ownAccount outside Lambda should be empty")
	}
	if err := checkSweepTarget("942542972736", ""); err == nil || !strings.Contains(err.Error(), "942542972736") {
		t.Errorf("forbidden account accepted with no own account known: %v", err)
	}
	if err := checkSweepTarget("435415984226", ""); err != nil {
		t.Errorf("member account refused: %v", err)
	}
}

// Every AssumeRole the orchestrator makes goes through the check: the only
// call site in the package's non-test code is assumeSweepRoleWith.
func TestEveryAssumeRoleGoesThroughTheTargetCheck(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sites := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n) //nolint:gosec // package-local file
		if err != nil {
			t.Fatal(err)
		}
		c := strings.Count(string(b), ".AssumeRole(ctx")
		if c > 0 && n != "target_account.go" {
			t.Errorf("%s calls AssumeRole directly; use assumeSweepRole so the target account is checked (#1584)", n)
		}
		sites += c
	}
	if sites != 1 {
		t.Errorf("found %d AssumeRole call sites, want exactly 1 (assumeSweepRoleWith)", sites)
	}
}
