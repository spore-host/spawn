package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/provenance"
	"github.com/spore-host/spawn/pkg/taskproto"
)

var (
	resultsLifecycleBucket string
	resultsLifecycleRegion string
	resultsLifecycleApply  bool
	resultsLifecycleRemove bool
	resultsRetainOff       bool
)

var resultsLifecycleCmd = &cobra.Command{
	Use:   "lifecycle",
	Short: "Show, install or remove the tiering rules on a results bucket (dry run unless --apply)",
	Long: `Install spawn's tiering rules on a bucket (#1076, phase 3).

The rules move task outputs to cheaper storage by what they would cost to make
again, using the tags spawn writes at upload:

  spawn:regen-class=cheap      Standard-IA at 30 days, Glacier IR at 90
  spawn:regen-class=moderate   Standard-IA at 60 days

Each rule also requires spawn:retain=false and a size of at least 128 KiB. So
expensive and unclassified outputs, outputs you have marked with
'spawn results retain', outputs written before v0.133.0 and small files like
logs are never moved. The rules only tier: nothing is ever deleted, and Glacier
Instant Retrieval objects can still be read immediately.

Without --apply this is a dry run: it prints the rules, the bucket's other
lifecycle rules (kept exactly as they are) and whether anything would change.
With --apply it writes the configuration and reads it back to confirm. --remove
takes spawn's rules out again and leaves everything else.

By default it works on this account's results bucket in the configured region.
--bucket names any bucket you own, including your own output buckets. Run
'spawn results retention' first to see what the rules would move.

Tiering has costs, shown by the dry run: a request charge per object moved, a
per-GB charge to read a tiered object back, and AWS's minimum storage duration
(30 days in Standard-IA, 90 in Glacier IR).

Needs s3:GetLifecycleConfiguration and s3:PutLifecycleConfiguration on the
bucket. Every call names your account as the expected bucket owner, so a
mistyped bucket name that belongs to someone else fails instead of being
changed.`,
	Example: `  # What would change on this account's results bucket
  spawn results lifecycle

  # Install the rules there
  spawn results lifecycle --apply

  # Your own output bucket
  spawn results lifecycle --bucket my-lab-results --apply

  # Take spawn's rules out again
  spawn results lifecycle --remove --apply`,
	Args: cobra.NoArgs,
	RunE: runResultsLifecycle,
}

var resultsRetainCmd = &cobra.Command{
	Use:   "retain <s3-uri>...",
	Short: "Mark task outputs to keep (spawn:retain=true), or let the tiering rules apply again (--off)",
	Long: `Set spawn:retain on task outputs (#1076).

spawn:retain=true takes an output out of every tiering rule, and out of any
future deletion sweep. --off sets spawn:retain=false, which lets the rules apply.
A URI ending in "/" covers every object under that prefix.

--off is also how outputs written before spawn:retain existed (v0.132.x) are
opted in to tiering. They carry no spawn:retain tag, so no rule matches them, and
they are kept until you run this.

S3 replaces an object's whole tag set on every write, so this command reads the
existing tags and writes them back with only spawn:retain changed. Don't use a
bare 'aws s3api put-object-tagging' for this: it would wipe the regeneration
tags the rules match on. Only spawn outputs (objects tagged spawn:run-id) are
changed. Anything else is listed and left alone.

Needs s3:GetObjectTagging and s3:PutObjectTagging on the objects, and
s3:ListBucket for a prefix.`,
	Example: `  # Keep one output
  spawn results retain s3://my-lab-results/project-x/final.bam

  # Keep a whole run's outputs
  spawn results retain s3://my-lab-results/project-x/run-7/

  # Let older (v0.132.x) outputs be tiered
  spawn results retain --off s3://spawn-results-123456789012-us-east-1/outputs/`,
	Args: cobra.MinimumNArgs(1),
	RunE: runResultsRetain,
}

func init() {
	resultsLifecycleCmd.Flags().StringVar(&resultsLifecycleBucket, "bucket", "", "Bucket to work on (default: spawn-results-<account>-<region>)")
	resultsLifecycleCmd.Flags().StringVar(&resultsLifecycleRegion, "region", "", "Region for the default bucket (default: configured region)")
	resultsLifecycleCmd.Flags().BoolVar(&resultsLifecycleApply, "apply", false, "Write the change (default: dry run)")
	resultsLifecycleCmd.Flags().BoolVar(&resultsLifecycleRemove, "remove", false, "Take spawn's rules out instead of installing them")
	resultsRetainCmd.Flags().BoolVar(&resultsRetainOff, "off", false, "Set spawn:retain=false, letting the tiering rules apply")
	resultsCmd.AddCommand(resultsLifecycleCmd, resultsRetainCmd)
}

func resultsClient(ctx context.Context, region string) (*aws.Client, error) {
	if region != "" {
		return aws.NewClientWithRegion(ctx, region)
	}
	return aws.NewClient(ctx)
}

func runResultsLifecycle(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := resultsClient(ctx, resultsLifecycleRegion)
	if err != nil {
		return fmt.Errorf("initialize AWS client: %w", err)
	}
	account, err := client.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("resolve your account (the expected bucket owner): %w", err)
	}
	cfg := client.Config()
	bucket := resultsLifecycleBucket
	if bucket == "" {
		bucket = taskproto.DefaultResultsBucket(account, cfg.Region)
	}
	api := provenance.LifecycleClient(ctx, cfg, bucket)
	plan, err := provenance.PlanLifecycle(ctx, api, bucket, account, resultsLifecycleRemove)
	if err != nil {
		return err
	}
	applied := false
	if resultsLifecycleApply && plan.Changed {
		if err := provenance.ApplyLifecycle(ctx, api, plan, account); err != nil {
			return err
		}
		applied = true
	}
	if spawnOutputFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			*provenance.LifecyclePlan
			Applied bool `json:"applied"`
		}{plan, applied})
	}
	renderLifecyclePlan(os.Stdout, plan, resultsLifecycleApply, applied)
	return nil
}

// renderLifecyclePlan writes the human plan. Split out so the wording is
// testable without AWS.
func renderLifecyclePlan(w io.Writer, p *provenance.LifecyclePlan, apply, applied bool) {
	mode := "dry run; nothing is changed. Add --apply to write it."
	if apply {
		mode = "--apply"
	}
	fmt.Fprintf(w, "spawn results lifecycle: s3://%s (%s)\n\n", p.Bucket, mode)
	switch {
	case !p.Changed && p.Remove:
		fmt.Fprintf(w, "spawn's tiering rules are not on this bucket. Nothing to remove.\n")
	case !p.Changed:
		fmt.Fprintf(w, "spawn's tiering rules are already installed and current. Nothing to change.\n")
	case p.Remove:
		fmt.Fprintf(w, "Removes spawn's %d tiering rule(s). Objects already moved stay where they are.\n", len(p.Current))
	case len(p.Current) > 0:
		fmt.Fprintf(w, "Updates spawn's tiering rules to the current ones.\n")
	default:
		fmt.Fprintf(w, "Installs spawn's tiering rules.\n")
	}
	if len(p.Desired) > 0 {
		fmt.Fprintf(w, "\nspawn's rules (every one requires spawn:retain=false and at least 128 KiB):\n")
		for _, r := range p.Desired {
			fmt.Fprintf(w, "  %-22s %s\n", *r.ID, provenance.DescribeRule(r))
		}
	}
	if len(p.Others) > 0 {
		fmt.Fprintf(w, "\nThis bucket's other lifecycle rules, kept exactly as they are (%d):\n", len(p.Others))
		for _, r := range p.Others {
			id := "(no id)"
			if r.ID != nil {
				id = *r.ID
			}
			fmt.Fprintf(w, "  %s\n", id)
		}
		fmt.Fprintf(w, "  Check none of them overlaps spawn's. S3 applies every rule that matches an\n")
		fmt.Fprintf(w, "  object, so another rule could expire an output spawn would only have tiered.\n")
	}
	if p.MinimumObjectSize != "" {
		fmt.Fprintf(w, "\nThe bucket's transition minimum-size setting (%s) is kept as it is.\n", p.MinimumObjectSize)
	}
	if len(p.Desired) > 0 && p.Changed {
		fmt.Fprintf(w, "\nWhat tiering costs (%s):\n", provenance.RequestPriceSource)
		fmt.Fprintf(w, "  - a request charge per object moved: $%.2f per 1,000 into Standard-IA, $%.2f per 1,000 into Glacier IR;\n",
			provenance.TransitionRequestUSD(provenance.StorageClassIA)*1000, provenance.TransitionRequestUSD(provenance.StorageClassGIR)*1000)
		fmt.Fprintf(w, "  - reading a tiered object back: $%.2f/GB from Standard-IA, $%.2f/GB from Glacier IR;\n",
			provenance.RetrievalUSDPerGB(provenance.StorageClassIA), provenance.RetrievalUSDPerGB(provenance.StorageClassGIR))
		fmt.Fprintf(w, "  - AWS's minimum storage duration: 30 days in Standard-IA, 90 in Glacier IR, billed in full if an\n")
		fmt.Fprintf(w, "    object is deleted or moved on sooner.\n")
		fmt.Fprintf(w, "  'spawn results retention' shows what the rules would move here and the monthly saving.\n")
	}
	if applied {
		fmt.Fprintf(w, "\nWritten, and read back to confirm. S3 runs lifecycle rules about once a day, so the first\n")
		fmt.Fprintf(w, "objects move within a day or two of reaching their age.\n")
	}
}

func runResultsRetain(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("initialize AWS client: %w", err)
	}
	cfg := client.Config()
	type uriResult struct {
		URI     string                    `json:"uri"`
		Results []provenance.RetainResult `json:"results"`
		Error   string                    `json:"error,omitempty"`
	}
	var all []uriResult
	failed := 0
	for _, uri := range args {
		bucket, key, err := provenance.SplitS3URI(uri)
		if err != nil {
			return err
		}
		res, err := provenance.SetRetain(ctx, provenance.LifecycleClient(ctx, cfg, bucket), bucket, key, !resultsRetainOff)
		ur := uriResult{URI: uri, Results: res}
		if err != nil {
			ur.Error = err.Error()
			failed++
		}
		for _, r := range res {
			if r.Outcome == provenance.RetainError {
				failed++
			}
		}
		all = append(all, ur)
	}
	if spawnOutputFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(all); err != nil {
			return err
		}
	} else {
		value := "true"
		if resultsRetainOff {
			value = "false"
		}
		for _, ur := range all {
			fmt.Fprintf(os.Stdout, "%s\n", ur.URI)
			if ur.Error != "" {
				fmt.Fprintf(os.Stdout, "  error: %s\n", ur.Error)
			}
			renderRetainResults(os.Stdout, ur.Results, value)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d object(s) or URI(s) could not be updated", failed)
	}
	return nil
}

// renderRetainResults summarises per-object outcomes, listing every object
// that wasn't set, with why.
func renderRetainResults(w io.Writer, res []provenance.RetainResult, value string) {
	counts := map[string]int{}
	for _, r := range res {
		counts[r.Outcome]++
		switch r.Outcome {
		case provenance.RetainNotOutput:
			fmt.Fprintf(w, "  left alone (no spawn:run-id, not a spawn output): %s\n", r.Key)
		case provenance.RetainError:
			fmt.Fprintf(w, "  FAILED %s: %s\n", r.Key, r.Error)
		}
	}
	fmt.Fprintf(w, "  spawn:retain=%s set on %d, already so on %d, left alone %d, failed %d\n",
		value, counts[provenance.RetainSet], counts[provenance.RetainUnchanged], counts[provenance.RetainNotOutput], counts[provenance.RetainError])
}
