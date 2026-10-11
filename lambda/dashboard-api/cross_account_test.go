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

// Cross-account reads must be made AS THE CALLER (#815).
//
// The member-account role's trust names `Cognito_SpawnDashboard_Auth_Role`, and
// that role carries `sts:AssumeRole` scoped to exactly the one role ARN. Both
// halves of the design were already deployed and correct; only the code was
// wrong — main.go built one aws.Config from the Lambda's own credentials and
// passed it everywhere, so the AssumeRole was attributed to the Lambda. Measured
// with simulate-principal-policy:
//
//	Cognito_SpawnDashboard_Auth_Role -> allowed
//	SpawnDashboardLambdaRole         -> implicitDeny
//
// which is why the dashboard's cross-account instance listing has never worked,
// and why the fix needed no IAM change at all.

// Without caller credentials, a cross-account read must REFUSE.
//
// The tempting fallback is this Lambda's own credentials. That cannot succeed
// (the trust does not name it) and, if the trust were widened to make it succeed,
// would create a standing role able to read every onboarded tenant's instances
// from behind a public API — the shape this audit spent its time removing.
func TestCrossAccountReadRefusesWithoutCallerCredentials(t *testing.T) {
	_, err := getEC2ClientForRegion(context.Background(), aws.Config{}, nil, "us-east-1")
	if err == nil {
		t.Fatal("getEC2ClientForRegion succeeded with no caller credentials; it must refuse rather " +
			"than fall back to this Lambda's role")
	}
	if !errors.Is(err, errNoCallerCredentials) {
		t.Errorf("error is not errNoCallerCredentials, so a caller cannot tell this apart from a "+
			"transport failure: %v", err)
	}
	for _, want := range []string{"caller", "credentials"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// With caller credentials, a client is built.
func TestCrossAccountReadBuildsAClientFromCallerCredentials(t *testing.T) {
	creds := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "s", SessionToken: "t"}, nil
	})
	c, err := getEC2ClientForRegion(context.Background(), aws.Config{}, creds, "eu-west-1")
	if err != nil {
		t.Fatalf("getEC2ClientForRegion with caller credentials: %v", err)
	}
	if c == nil {
		t.Fatal("returned a nil client with no error")
	}
}

// The Lambda's own credentials must not be used for the AssumeRole.
//
// Source-level because the distinction is which credentials the STS client
// carries, and that is not observable from the returned client. Anchored on the
// assignment, not the identifier (rule 3).
func TestAssumeRoleUsesTheCallerNotTheLambda(t *testing.T) {
	src := readDashboardFile(t, "cross_account.go")

	if !strings.Contains(src, "o.Credentials = callerCreds") {
		t.Error("the STS client is not built with the caller's credentials, so the AssumeRole is " +
			"attributed to this Lambda — which the member-account trust does not name (#815)")
	}
	// A nil-check must gate it, or a nil provider silently becomes "the Lambda's
	// default credentials" rather than a refusal.
	if !strings.Contains(src, "if callerCreds == nil") {
		t.Error("there is no nil guard on callerCreds; a nil provider would fall through to the " +
			"ambient credential chain, i.e. this Lambda's role")
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
