package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Instance reads are made with the caller's own credentials, in the caller's
// own account (#815, #1585). There is no role to assume:
// SpawnDashboardCrossAccountReadRole, which every identity-pool sign-in could
// assume from the browser, is retired.

// Without caller credentials, an instance read must REFUSE, not fall back to
// this Lambda's role, which would be a standing read of every tenant.
func TestInstanceReadRefusesWithoutCallerCredentials(t *testing.T) {
	_, err := getEC2ClientForRegion(context.Background(), nil, "us-east-1")
	if !errors.Is(err, errNoCallerCredentials) {
		t.Fatalf("getEC2ClientForRegion with no caller credentials: %v, want errNoCallerCredentials", err)
	}
	for _, want := range []string{"caller", "credentials"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Each route refuses before fanning out over the regions.
	if _, err := listInstances(context.Background(), nil, "arn:aws:iam::1:user/a", ""); !errors.Is(err, errNoCallerCredentials) {
		t.Errorf("listInstances: %v", err)
	}
	if _, err := getInstance(context.Background(), nil, "i-1", "arn:aws:iam::1:user/a"); !errors.Is(err, errNoCallerCredentials) {
		t.Errorf("getInstance: %v", err)
	}
	if _, err := calculateInstanceCosts(context.Background(), nil, "arn:aws:iam::1:user/a"); !errors.Is(err, errNoCallerCredentials) {
		t.Errorf("calculateInstanceCosts: %v", err)
	}
}

// With caller credentials, the client signs with exactly those: no AssumeRole,
// no other identity.
func TestInstanceReadUsesTheCallersCredentials(t *testing.T) {
	creds := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIACALLER", SecretAccessKey: "s", SessionToken: "t"}, nil
	})
	c, err := getEC2ClientForRegion(context.Background(), creds, "eu-west-1")
	if err != nil {
		t.Fatalf("getEC2ClientForRegion with caller credentials: %v", err)
	}
	got, err := c.Options().Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessKeyID != "AKIACALLER" {
		t.Errorf("client signs as %q, want the caller's AKIACALLER", got.AccessKeyID)
	}
	if r := c.Options().Region; r != "eu-west-1" {
		t.Errorf("client region %q", r)
	}
}

// The read role is gone from the code, not just unused: no AssumeRole
// provider, no role ARN, no override variable (#1585).
func TestNoCrossAccountReadRoleRemains(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src := readDashboardFile(t, n)
		for _, banned := range []string{"stscreds", "AssumeRole(", "NewAssumeRoleProvider", "SPAWN_DASHBOARD_CROSS_ACCOUNT_ROLE", "role/SpawnDashboardCrossAccountReadRole"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s still contains %q: dashboard-api reads EC2 only with the caller's own credentials", n, banned)
			}
		}
	}
	if _, err := os.Stat(filepath.Join("scripts", "setup-cross-account-role.sh")); err == nil {
		t.Error("scripts/setup-cross-account-role.sh still exists: it creates the retired read role")
	}
}

// An identity-pool session is never acted as: its role has no EC2 access.
func TestIsIdentityPoolSession(t *testing.T) {
	for in, want := range map[string]bool{
		"arn:aws:sts::966362334030:assumed-role/Cognito_SpawnDashboard_Auth_Role/CognitoIdentityCredentials": true,
		"arn:aws:sts::111111111111:assumed-role/spore-portal-launch/alice":                                   false,
		"arn:aws:iam::111111111111:user/CognitoIdentityCredentials":                                          false,
		"": false,
	} {
		if got := isIdentityPoolSession(in); got != want {
			t.Errorf("isIdentityPoolSession(%q) = %v, want %v", in, got, want)
		}
	}
}

// Terminate is not done from the dashboard (spawn#1349, permanent): the 403
// names the CLI, and the handler takes no credentials, so it can make no call.
func TestTerminateIsRefusedAndNamesTheCLI(t *testing.T) {
	resp, err := handleTerminateInstance("i-0abc")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 || !strings.Contains(resp.Body, "spawn terminate i-0abc") {
		t.Errorf("status %d body %q; want 403 naming `spawn terminate i-0abc`", resp.StatusCode, resp.Body)
	}
}

// Every failure path in the credentials handler must return a NIL provider.
//
// Returning a half-built provider from a path where validation failed would let a
// caller we could not verify be acted as — the inverse of the bug, and worse.
func TestUnvalidatedCallersAreNotActableAs(t *testing.T) {
	src := readDashboardFile(t, "auth.go")

	i := strings.Index(src, "func getUserFromCredentialsHeader(")
	if i < 0 {
		t.Fatal("getUserFromCredentialsHeader not found")
	}
	body := src[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}

	// The provider is assigned exactly once, and only after the JSON parsed.
	assign := strings.Index(body, "callerCreds = aws.CredentialsProviderFunc")
	if assign < 0 {
		t.Fatal("callerCreds is never assigned a provider")
	}
	parse := strings.Index(body, "json.Unmarshal(credsJSON")
	if parse < 0 || parse > assign {
		t.Error("the provider is built before the credentials JSON is parsed")
	}
	// Declared before the first return, so early failures hand back nil.
	decl := strings.Index(body, "var callerCreds aws.CredentialsProvider")
	firstReturn := strings.Index(body, "return \"\", \"\", \"\",")
	if decl < 0 {
		t.Fatal("callerCreds is not declared")
	}
	if firstReturn >= 0 && decl > firstReturn {
		t.Error("callerCreds is declared after the first early return, so some failure paths cannot " +
			"return a nil provider")
	}
}

func readDashboardFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", name)) //nolint:gosec // fixed package-local path
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
