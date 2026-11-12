package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// `spawn onboard` creates a role trusting a spore.host principal. It must refuse,
// before any IAM call, when either end is 942542972736 or an Organizations
// management account, and when the onboarded account's organization cannot be
// read (#1582).
func TestOnboardRefusesForbiddenAndManagementAccounts(t *testing.T) {
	cases := []struct {
		name   string
		tweak  func()
		reason string
	}{
		{"credentials in 942542972736", func() {
			onboardCaller = func(context.Context, aws.Config) (string, string, error) {
				return "942542972736", "arn:aws:iam::942542972736:user/tester", nil
			}
		}, "942542972736"},
		{"trusting a role in 942542972736", func() {
			onboardPhoneHomeRole = "arn:aws:iam::942542972736:role/portal-phone-home"
		}, "942542972736"},
		{"a management account", func() {
			onboardManagementAccount = func(context.Context, aws.Config) (string, error) { return "222222222222", nil }
		}, "management account"},
		{"an organization that cannot be read", func() {
			onboardManagementAccount = func(context.Context, aws.Config) (string, error) {
				return "", errors.New("AccessDeniedException")
			}
		}, "organizations:DescribeOrganization"},
		{"a trusted principal with no account", func() {
			onboardPhoneHomeRole = "portal-phone-home"
		}, "not a 12-digit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			onboardHermeticTweak = c.tweak
			t.Cleanup(func() { onboardHermeticTweak = nil })
			fake := &fakeOnboardIAM{}
			r := runOnboardHermetic(t, fake, "us-east-1", "", "")
			if r.err == nil || !strings.Contains(r.err.Error(), c.reason) {
				t.Fatalf("err = %v, want a refusal mentioning %q", r.err, c.reason)
			}
			if len(fake.created)+len(fake.trustUpdates)+len(fake.puts) != 0 {
				t.Errorf("IAM writes despite the refusal: %d creates, %d trust updates, %d puts",
					len(fake.created), len(fake.trustUpdates), len(fake.puts))
			}
			if len(r.http.reqs) != 0 {
				t.Errorf("the refused onboarding still phoned home")
			}
		})
	}

	// And a member account goes ahead.
	fake := &fakeOnboardIAM{}
	if r := runOnboardHermetic(t, fake, "us-east-1", "", ""); r.err != nil {
		t.Fatalf("a member account was refused: %v", r.err)
	}
}

// Every command that creates a cross-account role checks the boundary first:
// the call to checkCrossAccountTrust precedes the call that writes the trust,
// in the same function. The writers are named here because the check is in
// cmd; pkg/aws's SpawnSweepCrossAccountRole has its own (aws.CheckSweepRoleTrust,
// pkg/aws and cmd sweep_role tests).
func TestCrossAccountTrustWritersCheckTheBoundary(t *testing.T) {
	writers := map[string]string{
		"onboard.go": "ensurePortalOnboardRole",
		"bot.go":     "EnsureCrossAccountRole",
	}
	for file, writer := range writers {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				return true
			}
			checkAt, writeAt := token.NoPos, token.NoPos
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				switch {
				case name == "checkCrossAccountTrust" && checkAt == token.NoPos:
					checkAt = call.Pos()
				case name == writer && writeAt == token.NoPos:
					writeAt = call.Pos()
				}
				return true
			})
			if writeAt == token.NoPos {
				return true
			}
			found = true
			if checkAt == token.NoPos || checkAt > writeAt {
				t.Errorf("%s: %s calls %s without checkCrossAccountTrust before it (#1582)", file, fd.Name.Name, writer)
			}
			return true
		})
		if !found {
			t.Errorf("%s: no call to %s found; the discovery has broken, not the rule", file, writer)
		}
	}
}
