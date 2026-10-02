package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

// ReaperCoverageRoleName is the conventional name of the role an operator creates
// in a spore-launching account to let the out-of-band reaper reach it
// (lambda/ttl-reaper/README.md). Its presence is evidence of coverage: nothing
// else creates it.
const ReaperCoverageRoleName = "spawn-ttl-reaper-ec2"

// reaperFunctionPrefix matches the reaper Lambda's name when it runs in THIS
// account (the REAPER_SCAN_SELF deployment, e.g. "spawn-ttl-reaper-production").
const reaperFunctionPrefix = "spawn-ttl-reaper"

// ReaperCoverage is what could be determined about whether an out-of-band reaper
// will reclaim this account's expired instances.
type ReaperCoverage struct {
	// Covered is true only when positive evidence was found. False with
	// Determined=true means "looked, and there is none".
	Covered bool
	// Determined is false when neither probe could be completed (e.g. no
	// permission), in which case nothing may be claimed either way.
	Determined bool
	// How names the evidence, e.g. "in-account reaper spawn-ttl-reaper-production"
	// or "cross-account role spawn-ttl-reaper-ec2".
	How string
	// Why explains an undetermined result.
	Why string
}

// Summary renders the coverage verdict as one line for a human.
func (c ReaperCoverage) Summary() string {
	switch {
	case c.Covered:
		return "covered (" + c.How + ")"
	case c.Determined:
		return "NOT covered — no reaper runs here and no " + ReaperCoverageRoleName + " role grants one access"
	default:
		return "could not be determined (" + c.Why + ")"
	}
}

// DetectReaperCoverage reports whether an out-of-band TTL reaper will reclaim this
// account's expired instances.
//
// Why this is checkable at all: the previous implementation was a hardcoded
// "not detected" that always produced the same warning regardless of reality
// (spawn#624), on the reasoning that a launch account cannot see the reaper. But
// coverage leaves local evidence, in exactly two shapes:
//
//  1. The reaper runs IN this account (REAPER_SCAN_SELF) — a Lambda named
//     spawn-ttl-reaper-*.
//  2. The reaper runs elsewhere and was granted access here — the conventional
//     ReaperCoverageRoleName role, which nothing but that setup creates.
//
// Verified against three real accounts: the infra account matches (1), a
// spore-launching account matches (2), and an account with neither matches
// neither — which was the account where a TTL-expired instance billed for 13 days
// while `spawn doctor` said exactly what it says everywhere else.
//
// A constant warning is one users learn to ignore; that is the defect being fixed,
// so an undetermined result is reported as undetermined rather than as "not
// covered".
func DetectReaperCoverage(ctx context.Context, cfg awssdk.Config) ReaperCoverage {
	var probeErrs []string

	// 1. A reaper running in this account.
	lam := lambda.NewFromConfig(cfg)
	var marker *string
	for page := 0; page < 10; page++ { // bounded: an account with 500+ functions is not worth paging forever
		out, err := lam.ListFunctions(ctx, &lambda.ListFunctionsInput{Marker: marker})
		if err != nil {
			probeErrs = append(probeErrs, "lambda:ListFunctions: "+err.Error())
			break
		}
		for _, fn := range out.Functions {
			if strings.HasPrefix(awssdk.ToString(fn.FunctionName), reaperFunctionPrefix) {
				return ReaperCoverage{
					Covered:    true,
					Determined: true,
					How:        "in-account reaper " + awssdk.ToString(fn.FunctionName),
				}
			}
		}
		if out.NextMarker == nil || awssdk.ToString(out.NextMarker) == "" {
			break
		}
		marker = out.NextMarker
	}

	// 2. A role here granting a reaper elsewhere access.
	iamCli := iam.NewFromConfig(cfg)
	_, err := iamCli.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(ReaperCoverageRoleName)})
	switch {
	case err == nil:
		return ReaperCoverage{
			Covered:    true,
			Determined: true,
			How:        "cross-account role " + ReaperCoverageRoleName,
		}
	case isNoSuchEntity(err):
		// Definitive absence. Combined with probe 1, this is a real "not covered"
		// — but only if probe 1 actually ran.
		if len(probeErrs) == 0 {
			return ReaperCoverage{Determined: true}
		}
	default:
		probeErrs = append(probeErrs, "iam:GetRole: "+err.Error())
	}

	return ReaperCoverage{Why: strings.Join(probeErrs, "; ")}
}

func isNoSuchEntity(err error) bool {
	var nse *iamtypes.NoSuchEntityException
	return errors.As(err, &nse)
}

// ReaperCoverageAdvice is the sentence to show a user whose launch depends on the
// out-of-band reaper in an account that has none. It names what will not be
// reclaimed rather than describing the mechanism.
func ReaperCoverageAdvice(c ReaperCoverage, onComplete, fsxLifecycle string) string {
	if c.Covered {
		return ""
	}

	var at []string
	if strings.EqualFold(onComplete, "stop") || strings.EqualFold(onComplete, "hibernate") {
		at = append(at, fmt.Sprintf("--on-complete %s leaves this instance's EBS volumes (and any Elastic IP) billing after the workload finishes", strings.ToLower(onComplete)))
	}
	if strings.EqualFold(fsxLifecycle, "ephemeral") {
		at = append(at, "--fsx-lifecycle ephemeral relies entirely on that reaper to delete the filesystem — nothing else will, so a 1200 GiB minimum filesystem (~$174/month) would persist")
	}
	if len(at) == 0 {
		return ""
	}

	lead := "⚠️  No out-of-band TTL reaper covers this account"
	if !c.Determined {
		lead = "⚠️  Could not confirm an out-of-band TTL reaper covers this account"
	}
	return lead + ", and:\n   - " + strings.Join(at, "\n   - ") +
		"\n   spored enforces lifetime from INSIDE the instance, so it cannot reclaim anything that outlives\n" +
		"   it — a stopped instance's volumes, or a filesystem that survives termination — and it enforces\n" +
		"   nothing at all if it dies. See docs/safety."
}
