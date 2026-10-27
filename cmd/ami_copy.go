package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	amiCopyRegion      string
	amiCopyToRegion    string
	amiCopyName        string
	amiCopyKMSKey      string
	amiCopyWait        bool
	amiCopyWaitTimeout time.Duration
)

// amiCopyPollInterval is how often --wait checks the copy.
const amiCopyPollInterval = 15 * time.Second

var amiCopyCmd = &cobra.Command{
	Use:   "copy <ami-id|name> --to-region <region>",
	Short: "Copy a spawn-managed AMI to another region",
	Long: `Copy a spawn-managed AMI to another region, keeping its spawn tags on the new
AMI and on every snapshot backing it, plus spawn:copied-from=<ami-id>@<region>.
The copy is spawn-managed like its source: 'spawn ami list --region <region>'
lists it, 'spawn footprint' counts it and its snapshots, and
'spawn ami delete <copy-id> --region <region>' removes it.

Only AMIs spawn created (tagged spawn:managed=true) can be copied.

Encryption: an encrypted source is copied encrypted with the target region's
default EBS key (aws/ebs), because a KMS key cannot be used outside its own
region. --kms-key names a key in the target region instead, and also encrypts
the copy of an unencrypted source.

The copy is independent of its source: deleting the source does NOT delete
its copies. Delete each copy in its own region.

Needs ec2:DescribeImages in the source region and ec2:CopyImage and
ec2:CreateTags in the target region.

Examples:
  spawn ami copy ami-0123456789abcdef0 --to-region us-east-2
  spawn ami copy pytorch-2.4-cuda12 --region us-east-1 --to-region us-west-2 --wait
  spawn ami copy ami-0123456789abcdef0 --to-region eu-west-1 --kms-key alias/research`,
	Args: cobra.ExactArgs(1),
	RunE: runAMICopy,
}

func runAMICopy(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}
	srcRegion := amiCopyRegion
	if srcRegion == "" {
		cfg, err := client.GetConfig(ctx)
		if err != nil {
			return fmt.Errorf("failed to get AWS config: %w", err)
		}
		srcRegion = cfg.Region
	}

	res, err := client.CopyAMI(ctx, aws.CopyAMIInput{
		SourceAMI:    args[0],
		SourceRegion: srcRegion,
		TargetRegion: amiCopyToRegion,
		Name:         amiCopyName,
		KMSKeyID:     amiCopyKMSKey,
	})
	if err != nil {
		var ade *aws.AccessDeniedError
		if errors.As(err, &ade) {
			return fmt.Errorf("%w\nspawn cannot grant this itself yet: ask your administrator for ec2:DescribeImages, ec2:CopyImage and ec2:CreateTags", err)
		}
		return err
	}
	asJSON := getOutputFormat() == "json"
	if !asJSON {
		fmt.Fprintf(os.Stderr, "Copying %s (%s) to %s as %s\n", res.SourceAMIID, res.SourceRegion, res.TargetRegion, res.AMIID)
	}

	waitErr := error(nil)
	if amiCopyWait {
		waitErr = client.WaitForAMICopy(ctx, res.TargetRegion, res.AMIID, aws.AMIWaitOptions{
			Interval: amiCopyPollInterval,
			MaxPolls: amiCopyMaxPolls(amiCopyWaitTimeout, amiCopyPollInterval),
			Progress: func(state string, poll, maxPolls int) {
				if !asJSON {
					fmt.Fprintf(os.Stderr, "\r  %s: %s (check %d of %d)   ", res.AMIID, state, poll, maxPolls)
				}
			},
		})
		if !asJSON {
			fmt.Fprintln(os.Stderr)
		}
	}

	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			return err
		}
	} else {
		printAMICopyResult(res, amiCopyWait && waitErr == nil)
	}
	if errors.Is(waitErr, aws.ErrAMIWaitExhausted) {
		return fmt.Errorf("%w; the copy continues, check it with 'spawn ami list --region %s'", waitErr, res.TargetRegion)
	}
	return waitErr
}

// amiCopyMaxPolls turns --wait-timeout into a poll count, at least one.
func amiCopyMaxPolls(timeout, interval time.Duration) int {
	n := int(timeout / interval)
	if n < 1 {
		n = 1
	}
	return n
}

func printAMICopyResult(res *aws.CopyAMIResult, available bool) {
	state := "pending"
	if available {
		state = "available"
	}
	fmt.Printf("AMI %s in %s (%s)\n", res.AMIID, res.TargetRegion, state)
	fmt.Printf("  copied from: %s@%s\n", res.SourceAMIID, res.SourceRegion)
	fmt.Printf("  name:        %s\n", res.Name)
	if res.Encrypted {
		key := res.KMSKeyID
		if key == "" {
			key = "default EBS key (aws/ebs) in " + res.TargetRegion
		}
		fmt.Printf("  encrypted:   %s\n", key)
	}
	fmt.Printf("  delete with: spawn ami delete %s --region %s\n", res.AMIID, res.TargetRegion)
}

func init() {
	amiGroupCmd.AddCommand(amiCopyCmd)
	amiCopyCmd.Flags().StringVar(&amiCopyRegion, "region", "", "Region the source AMI is in (default: current region from AWS config)")
	amiCopyCmd.Flags().StringVar(&amiCopyToRegion, "to-region", "", "Region to copy the AMI to (required)")
	_ = amiCopyCmd.MarkFlagRequired("to-region")
	amiCopyCmd.Flags().StringVar(&amiCopyName, "name", "", "Name for the copy (default: the source AMI's name)")
	amiCopyCmd.Flags().StringVar(&amiCopyKMSKey, "kms-key", "", "KMS key in the target region to encrypt the copy with (default: aws/ebs if the source is encrypted)")
	amiCopyCmd.Flags().BoolVar(&amiCopyWait, "wait", false, "Wait for the copy to become available")
	amiCopyCmd.Flags().DurationVar(&amiCopyWaitTimeout, "wait-timeout", time.Hour, "How long --wait waits before giving up (the copy itself continues)")
}
