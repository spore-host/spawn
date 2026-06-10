package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

var amiSnapshotsRegion string

var amiSnapshotsCmd = &cobra.Command{
	Use:   "snapshots <ami-id>",
	Short: "List the EBS snapshots backing an AMI",
	Long: `Show the EBS snapshots that back an AMI, with size, state, and whether each
snapshot is shared with other AMIs (which is why 'spawn ami delete' keeps shared
snapshots instead of deleting them).

Examples:
  spawn ami snapshots ami-0123456789abcdef0
  spawn ami snapshots ami-0123456789abcdef0 -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runAMISnapshots,
}

func runAMISnapshots(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	snaps, err := client.GetAMISnapshots(ctx, amiSnapshotsRegion, args[0])
	if err != nil {
		return err
	}

	if getOutputFormat() == "json" {
		return json.NewEncoder(os.Stdout).Encode(snaps)
	}

	if len(snaps) == 0 {
		fmt.Printf("AMI %s has no backing EBS snapshots.\n", args[0])
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()
	_, _ = fmt.Fprintf(w, "SNAPSHOT\tSIZE\tSTATE\tENCRYPTED\tAGE\tSHARED\n")
	for _, s := range snaps {
		shared := "exclusive"
		if len(s.SharedWith) > 0 {
			shared = strings.Join(s.SharedWith, ", ")
		}
		enc := "no"
		if s.Encrypted {
			enc = "yes"
		}
		age := "-"
		if !s.StartTime.IsZero() {
			age = formatDuration(time.Since(s.StartTime))
		}
		_, _ = fmt.Fprintf(w, "%s\t%dGB\t%s\t%s\t%s\t%s\n",
			s.SnapshotID, s.VolumeSize, s.State, enc, age, shared)
	}
	return nil
}

func init() {
	amiGroupCmd.AddCommand(amiSnapshotsCmd)
	amiSnapshotsCmd.Flags().StringVar(&amiSnapshotsRegion, "region", "", "AWS region (default: current region from AWS config)")
}
