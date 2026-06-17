package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	cbpRegion        string
	cbpPlatform      string
	cbpInstanceType  string
	cbpInstanceCount int32
	cbpDurationHours int32
	cbpDryRun        bool
	cbpTags          []string
)

var capacityBlockCmd = &cobra.Command{
	Use:   "capacity-block",
	Short: "Purchase and manage EC2 Capacity Blocks for ML",
	Long: `Purchase and manage EC2 Capacity Blocks for ML.

Discover purchasable offerings with 'truffle capacity-blocks', then purchase one
here. After purchase, launch into it with:
  spawn launch <name> --reservation-id <id> --capacity-block --az <reservation-az>`,
}

var capacityBlockPurchaseCmd = &cobra.Command{
	Use:   "purchase <offering-id>",
	Short: "Purchase a Capacity Block for ML (NON-REFUNDABLE up-front charge)",
	Long: `Purchase a Capacity Block for ML from an offering id (from 'truffle capacity-blocks').

⚠️  A Capacity Block is billed UP FRONT and is NON-REFUNDABLE — the full block
duration is charged at purchase. This is the single most expensive action spawn
can take. The purchase requires you to TYPE three confirmations (the exact price,
'purchase <offering-id>', and an acknowledgement phrase) and refuses to run on a
non-interactive terminal. Use --dry-run first to preview the price and terms
without buying anything.

The offering's instance type, count, and duration must be supplied so the exact
offering can be re-validated (and its current price re-confirmed) immediately
before purchase.`,
	Args: cobra.ExactArgs(1),
	RunE: runCapacityBlockPurchase,
}

func init() {
	rootCmd.AddCommand(capacityBlockCmd)
	capacityBlockCmd.AddCommand(capacityBlockPurchaseCmd)

	f := capacityBlockPurchaseCmd.Flags()
	f.StringVar(&cbpRegion, "region", "", "AWS region of the offering (required)")
	f.StringVar(&cbpInstanceType, "instance-type", "", "Instance type of the offering, e.g. p5.48xlarge (required)")
	f.Int32Var(&cbpInstanceCount, "count", 1, "Number of instances in the block")
	f.Int32Var(&cbpDurationHours, "duration-hours", 0, "Capacity Block duration in hours (required)")
	f.StringVar(&cbpPlatform, "platform", "Linux/UNIX", "Instance platform (Linux/UNIX, Windows, ...)")
	f.BoolVar(&cbpDryRun, "dry-run", false, "Preview the price and terms without purchasing (no charge, no write API call)")
	f.StringArrayVar(&cbpTags, "tag", nil, "Tag to apply to the reservation (key=value; repeatable)")
}

func runCapacityBlockPurchase(cmd *cobra.Command, args []string) error {
	offeringID := args[0]
	if cbpRegion == "" {
		return fmt.Errorf("--region is required")
	}
	if cbpInstanceType == "" {
		return fmt.Errorf("--instance-type is required (so the offering can be re-validated before purchase)")
	}
	if cbpDurationHours <= 0 {
		return fmt.Errorf("--duration-hours is required and must be > 0")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// 1. Resolve / re-validate the offering (read-only). Its price and terms are
	//    what we ask the user to confirm, and what we re-check before charging.
	off, err := client.FindCapacityBlockOffering(ctx, cbpRegion, offeringID, cbpInstanceType, cbpInstanceCount, cbpDurationHours)
	if err != nil {
		return err
	}

	// 2. Identify the purchasing account (shown in the confirmation).
	acctID, _ := client.GetAccountID(ctx) // best-effort; empty if unavailable

	printCapacityBlockSummary(off, acctID, cbpRegion)

	// 3. Dry-run stops here — no write API, no charge.
	if cbpDryRun {
		fmt.Fprintf(os.Stderr, "\n--dry-run: no purchase made. Re-run without --dry-run to buy (you will be asked to type three confirmations).\n")
		return nil
	}

	// 4. Three typed gates, in sequence. Refuse on non-interactive stdin — there
	//    is NO --yes bypass for an irreversible five-figure charge.
	if !stdinIsInteractive() {
		return fmt.Errorf("refusing to purchase a Capacity Block on a non-interactive terminal (this is a non-refundable charge requiring typed confirmation); run it interactively, or use --dry-run to preview")
	}

	reader := bufio.NewReader(os.Stdin)
	fee := off.UpfrontFee
	if !confirmTypedPhrase(reader,
		fmt.Sprintf("\nGate 1/3 — type the EXACT total up-front price to confirm (%s %s): ", fee, off.CurrencyCode),
		fee) {
		return fmt.Errorf("aborted: price not confirmed (gate 1/3) — nothing purchased")
	}
	if !confirmTypedPhrase(reader,
		fmt.Sprintf("Gate 2/3 — type 'purchase %s' to confirm the offering: ", offeringID),
		"purchase "+offeringID) {
		return fmt.Errorf("aborted: offering not confirmed (gate 2/3) — nothing purchased")
	}
	const ackPhrase = "I UNDERSTAND THIS IS NON-REFUNDABLE"
	if !confirmTypedPhrase(reader,
		fmt.Sprintf("Gate 3/3 — type '%s': ", ackPhrase),
		ackPhrase) {
		return fmt.Errorf("aborted: acknowledgement not given (gate 3/3) — nothing purchased")
	}

	// 5. Re-validate the offering/price immediately before charging — abort if it
	//    moved from what the user just confirmed.
	fresh, err := client.FindCapacityBlockOffering(ctx, cbpRegion, offeringID, cbpInstanceType, cbpInstanceCount, cbpDurationHours)
	if err != nil {
		return fmt.Errorf("re-validation before purchase failed (nothing purchased): %w", err)
	}
	if fresh.UpfrontFee != off.UpfrontFee || fresh.CurrencyCode != off.CurrencyCode {
		return fmt.Errorf("aborted: the offering price changed from %s %s to %s %s between confirmation and purchase — nothing purchased; re-run to see the new price",
			off.UpfrontFee, off.CurrencyCode, fresh.UpfrontFee, fresh.CurrencyCode)
	}

	tags, err := parseKVTags(cbpTags)
	if err != nil {
		return err
	}

	auditLog := audit.NewLogger(os.Stderr, acctID, offeringID)
	auditLog.LogOperationWithData("capacity_block_purchase", offeringID, "initiated",
		map[string]interface{}{"region": cbpRegion, "upfront_fee": off.UpfrontFee, "currency": off.CurrencyCode, "account": acctID}, nil)

	fmt.Fprintf(os.Stderr, "\nPurchasing Capacity Block %s ...\n", offeringID)
	reservationID, err := client.PurchaseCapacityBlock(ctx, cbpRegion, offeringID, cbpPlatform, false, tags)
	if err != nil {
		auditLog.LogOperationWithData("capacity_block_purchase", offeringID, "failed", map[string]interface{}{"region": cbpRegion}, err)
		return fmt.Errorf("purchase failed: %w", err)
	}
	auditLog.LogOperationWithData("capacity_block_purchase", offeringID, "success",
		map[string]interface{}{"reservation_id": reservationID, "region": cbpRegion, "upfront_fee": off.UpfrontFee, "currency": off.CurrencyCode}, nil)

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  ✅ Purchased Capacity Block\n")
	fmt.Printf("  Reservation ID:  %s\n", reservationID)
	fmt.Printf("  Region:          %s\n", cbpRegion)
	fmt.Printf("  Starts:          %s\n", off.StartDate)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("\n  Launch into it when the block starts:\n")
	fmt.Printf("    spawn launch <name> --reservation-id %s --capacity-block --az %s\n", reservationID, off.AvailabilityZone)
	return nil
}

func printCapacityBlockSummary(off *aws.CapacityBlockOffering, acctID, region string) {
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  Capacity Block for ML — purchase preview\n")
	fmt.Printf("  Offering ID:     %s\n", off.OfferingID)
	fmt.Printf("  Instance type:   %s ×%d\n", off.InstanceType, off.InstanceCount)
	fmt.Printf("  Region / AZ:     %s / %s\n", region, off.AvailabilityZone)
	fmt.Printf("  Starts:          %s\n", off.StartDate)
	fmt.Printf("  Ends:            %s\n", off.EndDate)
	fmt.Printf("  Duration:        %d hours\n", off.DurationHours)
	fmt.Printf("  Purchasing acct: %s\n", acctID)
	fmt.Printf("  UP-FRONT FEE:    %s %s  (NON-REFUNDABLE, charged at purchase)\n", off.UpfrontFee, off.CurrencyCode)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}
