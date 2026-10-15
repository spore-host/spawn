package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/spotrequest"
)

// Exit codes of `spored is-spot-replacement` (#1007). The bootstrap skips the
// user's workload on exitReplacement and runs it on anything else, so every
// failure mode, including an unknown subcommand on an older spored (cobra exits
// 1), lands on "run the workload", which is what the original always did.
const (
	exitReplacement    = 0
	exitNotReplacement = 1
	exitUnknown        = 2
)

// How long `spored is-spot-replacement` keeps retrying an inconclusive check
// before it answers "unknown". On an original instance the very first
// DescribeInstances answers (it is tagged spawn:managed=true), so this budget is
// only spent when EC2 or IMDS cannot be read. The bootstrap then runs the
// workload anyway, so the budget is a delay on a degraded launch, never a hold.
const (
	defaultReplacementCheckBudget = 60 * time.Second
	replacementCheckRetryInterval = 5 * time.Second
	replacementCheckCallTimeout   = 15 * time.Second
)

// replacementDetector answers once. Production reads IMDS and EC2; tests fake it.
type replacementDetector func(ctx context.Context) spotrequest.Detection

func newIsSpotReplacementCmd() *cobra.Command {
	var budget time.Duration
	cmd := &cobra.Command{
		Use:   "is-spot-replacement",
		Short: "Report whether this instance is a replacement launched from a leaked Spot request",
		Long: "Asks EC2 whether this instance was launched by a spawn persistent Spot request for a\n" +
			"different instance (#995, #1007). The bootstrap runs this before starting --command,\n" +
			"MPI, the task runner or custom user data, and skips them on a replacement.\n\n" +
			"Exit codes: 0 = replacement, 1 = not a replacement, 2 = unknown (could not tell).\n" +
			"Inconclusive reads are retried for up to --timeout before answering unknown.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(isSpotReplacement(cmd.Context(), cmd.OutOrStdout(), imdsReplacementDetector, budget, time.Now, sleepCtx))
		},
	}
	cmd.Flags().DurationVar(&budget, "timeout", defaultReplacementCheckBudget,
		"How long to retry an inconclusive check before answering unknown (exit 2)")
	return cmd
}

// isSpotReplacement runs the check and returns the process exit code. Pure apart
// from the injected detector, writer, clock and sleep, so the exit-code contract
// is testable without EC2.
func isSpotReplacement(ctx context.Context, w io.Writer, detect replacementDetector, budget time.Duration,
	now func() time.Time, sleep func(context.Context, time.Duration) bool) int {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := now().Add(budget)
	var d spotrequest.Detection
	for attempt := 1; ; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, replacementCheckCallTimeout)
		d = detect(callCtx)
		cancel()
		if d.Verdict != spotrequest.VerdictUnknown {
			break
		}
		if permanentDenial(d.Err) {
			_, _ = fmt.Fprintf(w, "spot replacement check: access denied, not retrying (%s)\n", d.Reason)
			break
		}
		if !now().Add(replacementCheckRetryInterval).Before(deadline) {
			break
		}
		_, _ = fmt.Fprintf(w, "spot replacement check inconclusive (attempt %d): %s; retrying\n", attempt, d.Reason)
		if !sleep(ctx, replacementCheckRetryInterval) {
			break
		}
	}

	_, _ = fmt.Fprintf(w, "%s: %s\n", d.Verdict, d.Reason)
	switch d.Verdict {
	case spotrequest.VerdictReplacement:
		return exitReplacement
	case spotrequest.VerdictNotReplacement:
		return exitNotReplacement
	default:
		return exitUnknown
	}
}

// permanentDenial reports an IAM refusal, which no retry will change. A role
// without ec2:DescribeInstances (a custom --iam-role) would otherwise spend the
// whole budget on every launch before the workload starts.
func permanentDenial(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	code := ae.ErrorCode()
	return code == "UnauthorizedOperation" || strings.HasPrefix(code, "AccessDenied")
}

// imdsReplacementDetector reads this instance's id and region from IMDS and asks
// spotrequest.DetectReplacement. Deliberately lighter than provider.NewEC2Provider:
// it needs no tag-driven config, and an IMDS failure must read as unknown rather
// than fall back to a local provider.
func imdsReplacementDetector(ctx context.Context) spotrequest.Detection {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return spotrequest.Detection{Verdict: spotrequest.VerdictUnknown, Reason: fmt.Sprintf("load AWS config: %v", err), Err: err}
	}
	doc, err := imds.NewFromConfig(cfg).GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return spotrequest.Detection{Verdict: spotrequest.VerdictUnknown, Reason: fmt.Sprintf("read instance identity from IMDS: %v", err), Err: err}
	}
	cfg.Region = doc.Region
	return spotrequest.DetectReplacement(ctx, ec2.NewFromConfig(cfg), doc.InstanceID)
}
