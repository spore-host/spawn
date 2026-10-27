package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/costmodel"
)

// statusNeedsInstance reports whether `spawn status` can ask the instance
// itself (spored, over SSH or SSM). Only a running instance has a spored to ask.
//
// Every other state used to fall through to the SSH path anyway. A stopped
// instance has no public IP, so the destination was "ec2-user@" — which ssh
// resolves to THIS machine — and status connected to the operator's own sshd
// (::1), offered it their keys, and failed with "Too many authentication
// failures" (#928). The question being asked of a stopped instance ("what is it
// costing me?") never needed the instance: EC2 and the tags spored keeps
// current answer it.
func statusNeedsInstance(instance *aws.InstanceInfo) bool {
	return instance != nil && instance.State == "running"
}

// ec2StatusReport is `spawn status` for an instance that is not running,
// assembled from EC2 data and the instance's tags alone — no SSH, no SSM.
// It is also the --output json document for that case: spored is not running,
// so there is no spored JSON to relay, and this says so with source "ec2".
type ec2StatusReport struct {
	Source       string `json:"source"` // always "ec2": built without reaching the instance
	InstanceID   string `json:"instance_id"`
	Name         string `json:"name,omitempty"`
	Region       string `json:"region"`
	InstanceType string `json:"instance_type"`
	Spot         bool   `json:"spot"`
	State        string `json:"state"`

	// StateReason is EC2's StateReason message; StateTransitionReason carries
	// the moment, from which StoppedSince is parsed.
	StateReason           string     `json:"state_reason,omitempty"`
	StateTransitionReason string     `json:"state_transition_reason,omitempty"`
	StoppedSince          *time.Time `json:"stopped_since,omitempty"`
	StoppedSeconds        float64    `json:"stopped_seconds,omitempty"`

	LaunchedAt *time.Time `json:"launched_at,omitempty"`
	// ElapsedSeconds is the wall-clock window EBS and Elastic IPs are priced
	// over: launch to now, or launch to termination for a terminated or
	// shutting-down instance (#1341).
	ElapsedSeconds float64 `json:"elapsed_seconds,omitempty"`
	// TerminatedAt is when a terminated or shutting-down instance's billing
	// stopped. TerminationTimeUnknown is set when it is gone but the time cannot
	// be found: wall-clock cost is then not counted, rather than run to now.
	TerminatedAt           *time.Time `json:"terminated_at,omitempty"`
	TerminationTimeUnknown bool       `json:"termination_time_unknown,omitempty"`

	TTLDeadline      *time.Time `json:"ttl_deadline,omitempty"`
	TTLRemainingSecs float64    `json:"ttl_remaining_seconds,omitempty"`
	TTLPastDue       bool       `json:"ttl_past_due,omitempty"`

	// ComputeSeconds is the cumulative running time across every boot, from
	// the spawn:compute-seconds checkpoint spored flushes when it stops.
	ComputeSeconds float64 `json:"compute_seconds"`
	ComputeKnown   bool    `json:"compute_known"`

	RatePerHour float64 `json:"rate_per_hour,omitempty"`
	RateBasis   string  `json:"rate_basis,omitempty"`
	ComputeCost float64 `json:"compute_cost"`

	// EBS bills on wall clock, stopped or not.
	EBSHourlyCost   float64 `json:"ebs_hourly_cost,omitempty"`
	EBSKnown        bool    `json:"ebs_known"`
	StorageCost     float64 `json:"storage_cost"`
	StorageThisStop float64 `json:"storage_cost_this_stop,omitempty"`
	// Public IPv4 (#51): auto-assigned addresses bill with compute (released on
	// stop), Elastic IPs on the wall clock. Known once spored has recorded them.
	PublicIPv4Count   int     `json:"public_ipv4_count,omitempty"`
	ElasticIPv4Count  int     `json:"elastic_ipv4_count,omitempty"`
	IPv4Known         bool    `json:"ipv4_known"`
	IPv4Cost          float64 `json:"ipv4_cost"`
	IPv4ThisStop      float64 `json:"ipv4_cost_this_stop,omitempty"`
	StoppedPerHour    float64 `json:"stopped_rate_per_hour,omitempty"`
	CostLimit         float64 `json:"cost_limit,omitempty"`
	CostLimitUsed     float64 `json:"cost_limit_used,omitempty"`
	CumulativeCost    float64 `json:"cumulative_cost"`
	CumulativeKnown   bool    `json:"cumulative_known"`
	EffectiveRate     float64 `json:"effective_rate_per_hour,omitempty"`
	EffectiveRateNote string  `json:"effective_rate_note,omitempty"`
}

// minEC2EffectiveRateWindow mirrors spored's: below a minute of elapsed time a
// rate is noise.
const minEC2EffectiveRateWindow = time.Minute

// buildEC2StatusReport assembles the report. now is injected for tests.
func buildEC2StatusReport(instance *aws.InstanceInfo, rate statusRate, now time.Time) ec2StatusReport {
	r := ec2StatusReport{
		Source:                "ec2",
		InstanceID:            instance.InstanceID,
		Name:                  instance.Name,
		Region:                instance.Region,
		InstanceType:          instance.InstanceType,
		Spot:                  instance.SpotInstance,
		State:                 instance.State,
		StateReason:           strings.TrimSpace(instance.StateReason),
		StateTransitionReason: strings.TrimSpace(instance.StateTransitionReason),
	}

	if instance.State == "stopped" || instance.State == "stopping" {
		since := parseTransitionTime(instance.StateTransitionReason)
		if since.IsZero() {
			// spawn stop records the moment too, for when EC2's reason has none.
			if t, err := time.Parse(time.RFC3339, instance.Tags["spawn:last-stop-time"]); err == nil {
				since = t.UTC()
			}
		}
		if !since.IsZero() && !since.After(now) {
			r.StoppedSince = &since
			r.StoppedSeconds = now.Sub(since).Seconds()
		}
	}

	// The ORIGINAL launch (spawn:launch-time); EC2's LaunchTime moves on every
	// start. The same anchor spawn extend uses (#992).
	launch := originalLaunchTime(instance)
	window := accrualEnd(instance, now)
	switch {
	case window.Ended && window.Known:
		t := window.End.UTC()
		r.TerminatedAt = &t
	case window.Ended:
		r.TerminationTimeUnknown = true
	}
	if !launch.IsZero() && !launch.After(now) {
		l := launch.UTC()
		r.LaunchedAt = &l
		if window.Known && window.End.After(launch) {
			r.ElapsedSeconds = window.End.Sub(launch).Seconds()
		}
	}

	if deadline, ok := lifecycleDeadline(instance); ok && !window.Ended {
		d := deadline.UTC()
		r.TTLDeadline = &d
		if rem := deadline.Sub(now); rem > 0 {
			r.TTLRemainingSecs = rem.Seconds()
		} else {
			r.TTLPastDue = true
		}
	}

	if v, ok := taggedComputeSeconds(instance); ok {
		r.ComputeSeconds = float64(v)
		r.ComputeKnown = true
	}

	r.RatePerHour, r.RateBasis = rate.PerHour, rate.Basis

	// Every figure through pkg/costmodel.Accrue, the function --cost-limit is
	// enforced with (#51); only the compute rate differs (a spot instance is
	// shown at its spot price, and enforced at on-demand, which can only
	// overstate it).
	compute := time.Duration(r.ComputeSeconds * float64(time.Second))
	elapsed := time.Duration(r.ElapsedSeconds * float64(time.Second))
	fp := footprintAt(instance, rate.PerHour)
	spend := costmodel.Accrue(fp, compute, elapsed)
	if rate.PerHour > 0 && r.ComputeKnown {
		r.ComputeCost = spend.Compute
	}
	if fp.EBSPerHour > 0 {
		r.EBSHourlyCost, r.EBSKnown = fp.EBSPerHour, true
		r.StorageCost = spend.EBS
	}
	r.IPv4Known = costmodel.IPv4Measured(instance.Tags, "spawn")
	r.PublicIPv4Count, r.ElasticIPv4Count = fp.PublicIPv4, fp.ElasticIPv4
	r.IPv4Cost = spend.PublicIPv4
	// What keeps billing while stopped: EBS and Elastic IPs (an auto-assigned
	// address is released on stop).
	stoppedFP := costmodel.Footprint{EBSPerHour: fp.EBSPerHour, ElasticIPv4: fp.ElasticIPv4}
	thisStop := costmodel.Accrue(stoppedFP, 0, time.Duration(r.StoppedSeconds*float64(time.Second)))
	r.StorageThisStop, r.IPv4ThisStop = thisStop.EBS, thisStop.PublicIPv4
	r.StoppedPerHour = stoppedFP.RunningPerHour()

	r.CumulativeKnown = rate.PerHour > 0 && r.ComputeKnown && r.EBSKnown && !r.TerminationTimeUnknown
	r.CumulativeCost = r.ComputeCost + r.StorageCost + r.IPv4Cost

	// The cap's own figure, at the enforced (on-demand) rate (#51).
	if limit := parseTagFloat(instance.Tags["spawn:cost-limit"]); limit > 0 && r.ComputeKnown {
		r.CostLimit = limit
		r.CostLimitUsed = costLimitSpend(instance, compute, elapsed).Total
	}

	if r.CumulativeKnown && r.ElapsedSeconds >= minEC2EffectiveRateWindow.Seconds() {
		r.EffectiveRate = r.CumulativeCost / (r.ElapsedSeconds / 3600)
		if rate.PerHour > 0 {
			continuous := fp.RunningPerHour()
			if saving := 1 - r.EffectiveRate/continuous; saving > 0 {
				r.EffectiveRateNote = fmt.Sprintf("%.0f%% lower than running continuously", saving*100)
			}
		}
	}
	return r
}

// renderEC2Status is the text form of the report.
func renderEC2Status(r ec2StatusReport, loc *time.Location) string {
	var b strings.Builder
	title := r.InstanceID
	if r.Name != "" {
		title = fmt.Sprintf("%s (%s)", r.Name, r.InstanceID)
	}
	spot := ""
	if r.Spot {
		spot = " (spot)"
	}
	fmt.Fprintf(&b, "Instance:         %s\n", title)
	fmt.Fprintf(&b, "Type:             %s%s, %s\n", r.InstanceType, spot, r.Region)
	fmt.Fprintf(&b, "State:            %s\n", colorizeInstanceState(r.State))
	if r.StoppedSince != nil {
		fmt.Fprintf(&b, "Stopped since:    %s, %s ago\n",
			formatStatusTime(*r.StoppedSince, loc), formatDuration(time.Duration(r.StoppedSeconds)*time.Second))
	}
	if r.StateReason != "" {
		fmt.Fprintf(&b, "Reason:           %s\n", r.StateReason)
	} else if r.StateTransitionReason != "" {
		fmt.Fprintf(&b, "Reason:           %s\n", r.StateTransitionReason)
	}
	switch {
	case r.TerminatedAt != nil:
		fmt.Fprintf(&b, "Terminated:       %s (nothing bills after this)\n", formatStatusTime(*r.TerminatedAt, loc))
	case r.TerminationTimeUnknown:
		fmt.Fprintf(&b, "Terminated:       time unknown (%s)\n", accrualUnknownReason)
	}
	switch {
	case r.LaunchedAt != nil && r.TerminatedAt != nil:
		fmt.Fprintf(&b, "Launched:         %s, ran %s to termination\n",
			formatStatusTime(*r.LaunchedAt, loc), formatDuration(time.Duration(r.ElapsedSeconds)*time.Second))
	case r.LaunchedAt != nil && r.TerminationTimeUnknown:
		fmt.Fprintf(&b, "Launched:         %s\n", formatStatusTime(*r.LaunchedAt, loc))
	case r.LaunchedAt != nil:
		fmt.Fprintf(&b, "Launched:         %s, %s ago\n",
			formatStatusTime(*r.LaunchedAt, loc), formatDuration(time.Duration(r.ElapsedSeconds)*time.Second))
	}

	switch {
	case r.TerminatedAt != nil || r.TerminationTimeUnknown:
		// Gone: no deadline left to enforce.
	case r.TTLDeadline != nil && r.TTLPastDue:
		fmt.Fprintf(&b, "TTL deadline:     %s, past due — it terminates as soon as it is started, or when the reaper next runs\n",
			formatStatusTime(*r.TTLDeadline, loc))
	case r.TTLDeadline != nil:
		fmt.Fprintf(&b, "TTL deadline:     %s, in %s (still counting while stopped)\n",
			formatStatusTime(*r.TTLDeadline, loc), formatDuration(time.Duration(r.TTLRemainingSecs)*time.Second))
	default:
		fmt.Fprintf(&b, "TTL deadline:     none recorded\n")
	}

	fmt.Fprintf(&b, "\nCost so far (from tags: the last checkpoint spored wrote before it stopped):\n")
	if r.ComputeKnown {
		compute := formatSecondsDuration(r.ComputeSeconds)
		if r.RatePerHour > 0 {
			fmt.Fprintf(&b, "  Compute:        $%.2f  (%s across all boots × $%.4f/hr, %s)\n",
				r.ComputeCost, compute, r.RatePerHour, r.RateBasis)
		} else {
			fmt.Fprintf(&b, "  Compute:        %s across all boots (rate unknown)\n", compute)
		}
	} else {
		fmt.Fprintf(&b, "  Compute:        — (no spawn:compute-seconds checkpoint recorded)\n")
	}
	switch {
	case r.EBSKnown && r.TerminationTimeUnknown:
		fmt.Fprintf(&b, "  Storage (EBS):  — (not counted: %s)\n", accrualUnknownReason)
	case r.EBSKnown && r.TerminatedAt != nil:
		fmt.Fprintf(&b, "  Storage (EBS):  $%.2f  (launch to termination × $%.4f/hr)\n",
			r.StorageCost, r.EBSHourlyCost)
	case r.EBSKnown:
		fmt.Fprintf(&b, "  Storage (EBS):  $%.2f  (since launch × $%.4f/hr; EBS bills whether or not it runs)\n",
			r.StorageCost, r.EBSHourlyCost)
	default:
		fmt.Fprintf(&b, "  Storage (EBS):  — (rate not yet recorded)\n")
	}
	switch {
	case r.IPv4Known && r.PublicIPv4Count+r.ElasticIPv4Count > 0:
		fmt.Fprintf(&b, "  Public IPv4:    $%.2f  (%d auto-assigned + %d Elastic × $%.3f/hr; Elastic IPs bill while stopped)\n",
			r.IPv4Cost, r.PublicIPv4Count, r.ElasticIPv4Count, costmodel.PublicIPv4PerHour)
	case !r.IPv4Known:
		fmt.Fprintf(&b, "  Public IPv4:    — (not recorded by spored, so not counted)\n")
	}
	if r.TerminationTimeUnknown && r.ElasticIPv4Count > 0 {
		fmt.Fprintf(&b, "                  Elastic IPs bill on the wall clock: not counted (%s)\n", accrualUnknownReason)
	}
	switch {
	case r.CumulativeKnown:
		fmt.Fprintf(&b, "  Total:          $%.2f\n", r.CumulativeCost)
	case r.TerminationTimeUnknown && r.ComputeKnown && r.RatePerHour > 0:
		fmt.Fprintf(&b, "  Total:          at least $%.2f  (storage and Elastic IPs not counted: termination time unknown)\n", r.CumulativeCost)
	}
	if r.CostLimit > 0 {
		fmt.Fprintf(&b, "  Cost limit:     $%.2f  ($%.2f counted at the on-demand rate, compute + EBS + public IPv4)\n",
			r.CostLimit, r.CostLimitUsed)
	}
	if r.EffectiveRate > 0 {
		note := ""
		if r.EffectiveRateNote != "" {
			note = "  (" + r.EffectiveRateNote + ")"
		}
		fmt.Fprintf(&b, "  Effective rate: $%.4f/hr%s\n", r.EffectiveRate, note)
	}

	if r.State == "stopped" || r.State == "stopping" {
		fmt.Fprintf(&b, "\nWhile stopped:\n")
		fmt.Fprintf(&b, "  Compute:        not billing\n")
		if r.EBSKnown {
			fmt.Fprintf(&b, "  Storage (EBS):  STILL BILLING ~$%.4f/hr (~$%.2f/day, ~$%.2f/month)",
				r.EBSHourlyCost, r.EBSHourlyCost*24, r.EBSHourlyCost*730)
			if r.StoppedSince != nil {
				fmt.Fprintf(&b, "; $%.2f during this stop", r.StorageThisStop)
			}
			fmt.Fprintln(&b)
		} else {
			fmt.Fprintf(&b, "  Storage (EBS):  still billing (rate not yet recorded)\n")
		}
		if r.ElasticIPv4Count > 0 {
			fmt.Fprintf(&b, "  Elastic IPs:    STILL BILLING %d × $%.3f/hr", r.ElasticIPv4Count, costmodel.PublicIPv4PerHour)
			if r.StoppedSince != nil {
				fmt.Fprintf(&b, "; $%.2f during this stop", r.IPv4ThisStop)
			}
			fmt.Fprintln(&b)
		}
		fmt.Fprintf(&b, "  spored is not running, so nothing on the instance enforces the deadline until it is\n")
		fmt.Fprintf(&b, "  started; the out-of-band reaper does, if it covers this account ('spawn doctor').\n")
		fmt.Fprintf(&b, "  'spawn start %s' to resume; 'spawn terminate %s' to stop all billing.\n", r.InstanceID, r.InstanceID)
	}
	if r.State == "pending" {
		fmt.Fprintf(&b, "\nThe instance is starting; spored's live status is available once it is running.\n")
	}
	return b.String()
}

// formatSecondsDuration renders a running time to the second, as spored's
// status does ("1m 2s"): compute on a short-lived instance is exactly the
// figure a minute-granular rendering rounds away.
func formatSecondsDuration(secs float64) string {
	d := time.Duration(secs) * time.Second
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// marshalEC2Status is the --output json form.
func marshalEC2Status(r ec2StatusReport) (string, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}
