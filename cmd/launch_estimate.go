package cmd

import (
	"fmt"
	"io"
	"time"
)

// renderInstanceCostEstimate writes the instance portion of the --estimate-only
// preview, for count instances.
//
// count was previously ignored (spawn#662): the estimate printed the
// per-instance rate times the TTL hours, so a 2-node cohort quoted $7.66/hr
// against a real ceiling of $15.31. That is the wrong direction to be wrong in —
// the flag exists to bound spend before committing, and it is reached by users
// who are being careful. It is also the same class as the FSx omission fixed in
// #613: a preview that understates is worse than no preview, because it is
// believed.
//
// With count == 1 (the default, and the overwhelmingly common case) the output is
// byte-for-byte what it was, so the common path gains no noise. Above 1, both
// figures are shown: the per-instance rate is what a user checks against a price
// list, and the total is what they actually pay.
func renderInstanceCostEstimate(w io.Writer, instanceType, region string, pricePerHour float64, sourceLabel, ttl string, count int) {
	// A count of 0 or less is reachable (`--count` is a plain int flag) and must
	// not multiply the estimate to zero: "$0.00" for a launch that still costs
	// money is the most dangerous possible rendering of this bug. Quote the
	// single-instance figure instead and let the launch path reject the value.
	effective := count
	if effective < 1 {
		effective = 1
	}

	total := pricePerHour * float64(effective)

	if effective == 1 {
		fmt.Fprintf(w, "💰 Cost estimate for %s in %s\n", instanceType, region)
		fmt.Fprintf(w, "   On-demand:  $%.4f/hr (%s)\n", pricePerHour, sourceLabel)
	} else {
		fmt.Fprintf(w, "💰 Cost estimate for %d × %s in %s\n", effective, instanceType, region)
		fmt.Fprintf(w, "   On-demand:  $%.4f/hr each — $%.4f/hr total (%s)\n",
			pricePerHour, total, sourceLabel)
	}

	if ttl == "" {
		return
	}
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return
	}
	// Render the duration, not a rounded hour count. "%.0f hr" turned --ttl 30m
	// into "0 hr", so the figure read as free next to a correct dollar amount —
	// reported against 0.118.0 as `TTL cost: $0.07 (0 hr × 2 instances)`. A cost
	// preview that says "0" about anything is the one number a reader will trust
	// and should not.
	if effective == 1 {
		fmt.Fprintf(w, "   TTL cost:   $%.2f (%s)\n", pricePerHour*d.Hours(), formatTTLDuration(d))
		return
	}
	fmt.Fprintf(w, "   TTL cost:   $%.2f (%s × %d instances)\n",
		total*d.Hours(), formatTTLDuration(d), effective)
}

// formatTTLDuration renders a TTL for the cost preview without rounding it away.
//
// time.Duration.String() gives "30m0s" and "1h30m0s"; the trailing zero units
// are noise in a one-line estimate. Hours are kept fractional when they are not
// whole, so a sub-hour TTL never reads as "0 hr" (#662 follow-up).
func formatTTLDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d hr", int(d.Hours()))
	default:
		return fmt.Sprintf("%.1f hr", d.Hours())
	}
}
