// Package trustboundary refuses cross-account trust spawn must never create.
//
// spawn writes trust across accounts in several places: SpawnSweepCrossAccountRole
// (pkg/aws, `spawn sweep setup` and detached sweeps), the spore-portal-onboard
// role (`spawn onboard`) and SpawnBotCrossAccount (`spawn notify register`).
// Every one of them must refuse two kinds of account, on either end of the
// trust, whatever the credentials allow:
//
//   - the accounts in forbidden, hardcoded on purpose: a configurable deny is
//     one a profile can switch off;
//   - the management account of an AWS Organization. A trust into or out of a
//     management account reaches every account in the organization.
//
// Fail closed. Whether an account is a management account is a question for
// AWS Organizations, asked with the credentials of the account in question.
// When it cannot be answered (DescribeOrganization denied, throttled, or
// failed), the trust is refused, with the permission named. The one answer
// that is not a refusal is the proven one: AWSOrganizationsNotInUseException,
// which pkg/orgaccount reports as "in no organization", in which case the
// account cannot be anyone's management account. An end for which spawn holds
// no credentials (the spore.host principal a role trusts) is checked against
// forbidden only; that is all a caller without its credentials can know.
//
// This package does not import the Organizations client, so pkg/aws, which
// the Lambdas import, need not carry it: callers pass a lookup (pkg/orgaccount
// in the CLI).
package trustboundary

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// forbidden are the accounts no spawn trust may be created in or name.
var forbidden = map[string]string{
	"942542972736": "an Amazon-internal account and the management account of AWS Organization " +
		"o-pygqyjjoym. No spore.host account may be trusted into it or out of it, and an ExternalId " +
		"or tag-gated least privilege does not change that: a trust created there once was " +
		"detected and force-denied in 12 minutes",
}

// Forbidden reports whether account is one no trust may involve, and why.
func Forbidden(account string) (string, bool) {
	why, bad := forbidden[strings.TrimSpace(account)]
	return why, bad
}

// ForbiddenAccounts returns the forbidden accounts, for templates and tests
// that must carry the same list.
func ForbiddenAccounts() []string {
	out := make([]string, 0, len(forbidden))
	for a := range forbidden {
		out = append(out, a)
	}
	return out
}

// ManagementLookup returns the management account of the AWS Organization
// the account it was made for belongs to, "" with no error when that account
// is in no organization, and an error when it cannot tell.
type ManagementLookup func(ctx context.Context) (string, error)

// Side is one end of a trust.
type Side struct {
	// What is a short description for messages: "the account the role is
	// created in", "the trusted account".
	What    string
	Account string
	// Mgmt asks Organizations about Account with Account's own credentials.
	// Nil when the caller holds none (only the forbidden list applies then).
	Mgmt ManagementLookup
}

// ErrOrganizationUnknown is wrapped by the refusal when an Organizations
// lookup failed.
var ErrOrganizationUnknown = errors.New("could not determine whether the account is an AWS Organizations management account")

// RefusedError is Check's refusal.
type RefusedError struct {
	Account string
	Why     string
	err     error
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("account %s: %s", e.Account, e.Why)
}

func (e *RefusedError) Unwrap() error { return e.err }

var accountRe = regexp.MustCompile(`^[0-9]{12}$`)

// AccountOfARN returns the account of an IAM or STS ARN
// (arn:<partition>:<service>::<account>:...), or "" if arn has none.
func AccountOfARN(arn string) string {
	parts := strings.SplitN(strings.TrimSpace(arn), ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || !accountRe.MatchString(parts[4]) {
		return ""
	}
	return parts[4]
}

// Check refuses a trust between the given sides when any of them is a
// forbidden account, is not an account at all, or is the management account
// of the organization any side with credentials belongs to; and refuses when
// any such organization cannot be read. nil means the trust may be created.
func Check(ctx context.Context, sides ...Side) error {
	if len(sides) == 0 {
		return &RefusedError{Why: "no accounts to check; refusing rather than assuming none is forbidden"}
	}
	for _, s := range sides {
		if !accountRe.MatchString(s.Account) {
			return &RefusedError{Account: s.Account, Why: fmt.Sprintf("%s is not a 12-digit AWS account ID, so it cannot be checked", s.What)}
		}
		if why, bad := Forbidden(s.Account); bad {
			return &RefusedError{Account: s.Account, Why: s.What + " is " + why}
		}
	}
	for _, s := range sides {
		if s.Mgmt == nil {
			continue
		}
		m, err := s.Mgmt(ctx)
		if err != nil {
			return &RefusedError{
				Account: s.Account,
				Why: fmt.Sprintf("spawn could not tell whether %s is an AWS Organizations management account (%v), "+
					"and creates no cross-account trust it cannot check. Grant organizations:DescribeOrganization "+
					"to the credentials for account %s; every member account may call it", s.What, err, s.Account),
				err: fmt.Errorf("%w: %v", ErrOrganizationUnknown, err),
			}
		}
		if m == "" {
			continue
		}
		for _, o := range sides {
			if o.Account == m {
				return &RefusedError{Account: m, Why: o.What + " is the management account of its AWS Organization. " +
					"A trust into or out of a management account reaches every account in the organization, " +
					"so spawn never creates one; use a member account"}
			}
		}
	}
	return nil
}
