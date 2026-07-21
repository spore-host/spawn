package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/doctor"
)

var doctorJSON bool

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that your environment is ready to launch a spore instance",
	Long: `Run a read-only preflight of everything a first launch needs: your tools,
AWS credentials, the resolved account and region, the EC2 and IAM permissions
spawn uses, a usable VPC/subnet, an SSH key, Session Manager, the spored instance
profile, and optional features (reaper backstop, Route 53).

Each check reports pass (✓), warning (⚠, optional feature unavailable), or fail
(✗, a core prerequisite is missing). It launches nothing and changes nothing.

If 'spawn doctor' passes, the Quick Start should work as written. On an
institution-managed AWS account, share the failing IAM checks with your cloud
administrator alongside the IAM baseline (docs: reference/iam-permissions).

Exit codes: 0 = ready (no failures), 1 = a core prerequisite failed.

Examples:
  spawn doctor
  spawn doctor --region us-west-2
  spawn doctor -o json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// Build a client pinned to the resolved region (empty = ambient chain),
		// same resolution spawn launch uses. A client-construction failure is
		// itself a finding, so surface it as a failed credentials check rather
		// than a Go error.
		client, err := aws.NewClientWithRegion(ctx, doctorRegion)
		report := (*doctor.Report)(nil)
		if err != nil {
			report = &doctor.Report{Checks: []doctor.Check{{
				Name:   "AWS credentials",
				Status: doctor.Fail,
				Fix:    "could not initialize an AWS client — run `aws login` or set a profile: " + err.Error(),
			}}}
		} else {
			report = doctor.Run(ctx, doctor.NewAWSProber(client, Version))
		}

		if doctorJSON || getOutputFormat() == "json" {
			if err := doctor.RenderJSON(os.Stdout, report); err != nil {
				return err
			}
		} else {
			doctor.RenderText(os.Stdout, report)
		}

		if !report.OK() {
			// The report is the user-facing output; exit non-zero (1) directly so
			// callers/CI can gate on readiness, without printing a second error line.
			os.Exit(1)
		}
		return nil
	},
}

var doctorRegion string

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.Flags().StringVar(&doctorRegion, "region", "", "AWS region to check (default: your resolved region)")
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "JSON output")
	_ = doctorCmd.Flags().MarkDeprecated("json", "use --output json instead")
}
