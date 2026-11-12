package trustboundary

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	acctA = "111122223333"
	acctB = "444455556666"
)

func lookup(m string, err error) ManagementLookup {
	return func(context.Context) (string, error) { return m, err }
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		sides   []Side
		refused string // the account refused, "" for none
		unknown bool   // wraps ErrOrganizationUnknown
	}{
		{"members of no organization", []Side{{What: "a", Account: acctA, Mgmt: lookup("", nil)}, {What: "b", Account: acctB}}, "", false},
		{"members of an organization", []Side{{What: "a", Account: acctA, Mgmt: lookup("777788889999", nil)}, {What: "b", Account: acctB}}, "", false},
		{"942542972736 created in", []Side{{What: "a", Account: "942542972736", Mgmt: lookup("", nil)}, {What: "b", Account: acctB}}, "942542972736", false},
		{"942542972736 trusted", []Side{{What: "a", Account: acctA, Mgmt: lookup("", nil)}, {What: "b", Account: "942542972736"}}, "942542972736", false},
		{"created in its own organization's management account", []Side{{What: "a", Account: acctA, Mgmt: lookup(acctA, nil)}, {What: "b", Account: acctB}}, acctA, false},
		{"trusting the organization's management account", []Side{{What: "a", Account: acctA, Mgmt: lookup(acctB, nil)}, {What: "b", Account: acctB}}, acctB, false},
		{"the trusted side's own organization", []Side{{What: "a", Account: acctA, Mgmt: lookup("", nil)}, {What: "b", Account: acctB, Mgmt: lookup(acctB, nil)}}, acctB, false},
		{"organization unreadable: fail closed", []Side{{What: "a", Account: acctA, Mgmt: lookup("", errors.New("AccessDeniedException"))}, {What: "b", Account: acctB}}, acctA, true},
		{"not an account", []Side{{What: "a", Account: "12345"}, {What: "b", Account: acctB}}, "12345", false},
		{"nothing to check", nil, "", false},
	}
	for _, c := range cases {
		err := Check(ctx, c.sides...)
		var refused *RefusedError
		switch {
		case c.name == "nothing to check":
			if !errors.As(err, &refused) {
				t.Errorf("%s: err = %v, want a refusal", c.name, err)
			}
		case c.refused == "":
			if err != nil {
				t.Errorf("%s: err = %v, want none", c.name, err)
			}
		case !errors.As(err, &refused) || refused.Account != c.refused:
			t.Errorf("%s: err = %v, want a refusal of %s", c.name, err, c.refused)
		case errors.Is(err, ErrOrganizationUnknown) != c.unknown:
			t.Errorf("%s: errors.Is(ErrOrganizationUnknown) = %v, want %v", c.name, !c.unknown, c.unknown)
		}
		if c.unknown && err != nil && !strings.Contains(err.Error(), "organizations:DescribeOrganization") {
			t.Errorf("%s: the refusal does not name the permission: %v", c.name, err)
		}
	}
}

// A forbidden account is refused before Organizations is asked anything.
func TestForbiddenNeedsNoLookup(t *testing.T) {
	asked := false
	err := Check(context.Background(),
		Side{What: "a", Account: acctA, Mgmt: func(context.Context) (string, error) { asked = true; return "", nil }},
		Side{What: "b", Account: "942542972736"})
	if err == nil || asked {
		t.Errorf("err = %v, asked = %v; want a refusal without a lookup", err, asked)
	}
}

func TestAccountOfARN(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:iam::966362334030:role/spore-portal-phone-home": "966362334030",
		"arn:aws-us-gov:iam::123456789012:role/x":                "123456789012",
		"arn:aws:sts::123456789012:assumed-role/r/s":             "123456789012",
		"arn:aws:iam::aws:policy/ReadOnlyAccess":                 "",
		"966362334030":                                           "",
		"":                                                       "",
		"arn:aws:iam::12345:role/x":                              "",
	} {
		if got := AccountOfARN(arn); got != want {
			t.Errorf("AccountOfARN(%q) = %q, want %q", arn, got, want)
		}
	}
}
