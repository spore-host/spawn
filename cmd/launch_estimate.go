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
	if effective == 1 {
		fmt.Fprintf(w, "   TTL cost:   $%.2f (%.0f hr)\n", pricePerHour*d.Hours(), d.Hours())
		return
	}
	fmt.Fprintf(w, "   TTL cost:   $%.2f (%.0f hr × %d instances)\n",
		total*d.Hours(), d.Hours(), effective)
}
