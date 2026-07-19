package taskproto

import (
	"encoding/json"
	"fmt"
)

// LaunchResult is what `spawn task run` reports after launching a task's
// instance. It intentionally carries instance_type and spot — which aws.LaunchResult
// does not — so an adapter has the placement facts without re-describing the
// instance.
type LaunchResult struct {
	TaskID       string `json:"task_id"`
	InstanceID   string `json:"instance_id"`
	Region       string `json:"region"`
	AZ           string `json:"az,omitempty"`
	InstanceType string `json:"instance_type"`
	Spot         bool   `json:"spot"`
}

// CompletionRecord is the durable, S3-persisted terminal record for a task,
// written by the on-instance wrapper to
// s3://<results-bucket>/tasks/<task_id>/completion.json. It is the signal
// adapters poll instead of SSH/SSM-polling an on-instance file. The JSON shape
// is authoritative — cross-language adapters parse this exact object.
//
// StartedAt/EndedAt are RFC3339 strings, not time.Time: the wrapper writes them
// with `date -u`, and cross-language adapters treat them as opaque timestamps.
// Keeping them as strings avoids a parse/format round-trip mismatch across the
// bash → Go → other-language boundary.
type CompletionRecord struct {
	TaskID       string     `json:"task_id"`
	ExitCode     int        `json:"exit_code"`
	State        TaskState  `json:"state"` // completed | failed
	StartedAt    string     `json:"started_at"`
	EndedAt      string     `json:"ended_at"`
	CostEstimate float64    `json:"cost_estimate,omitempty"`
	Logs         []string   `json:"logs,omitempty"` // s3:// URIs of command.log etc.
	RetryClass   RetryClass `json:"retry_class,omitempty"`
}

// ParseCompletionRecord unmarshals a CompletionRecord from the bytes of a
// completion.json object.
func ParseCompletionRecord(data []byte) (*CompletionRecord, error) {
	var rec CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse completion record: %w", err)
	}
	return &rec, nil
}

// RetryClass categorizes why a task ended, so an adapter can decide whether to
// retry. The JSON values are stable and shared across adapters.
type RetryClass string

const (
	RetryNone RetryClass = "" // success, or an unclassified terminal state

	// Wired in this increment:
	RetryAppError     RetryClass = "app_error"     // the user command exited non-zero
	RetryStagingError RetryClass = "staging_error" // an input/output S3 copy failed
	RetryCapacity     RetryClass = "capacity"      // no capacity at launch (see ClassifyLaunchError)

	// Defined but not yet populated — these need spored-side signals or
	// post-mortem instance inspection that a later increment adds:
	RetrySpotInterruption RetryClass = "spot_interruption"
	RetryInstanceHealth   RetryClass = "instance_health"
	RetryTTLExpired       RetryClass = "ttl_expired"
	RetryControllerLost   RetryClass = "controller_lost"
)

// Retryable reports whether a task in this class is worth retrying. Capacity and
// spot-interruption are transient (retry elsewhere/later); an app error or a
// staging error will recur unchanged, so they are not retryable. The stubbed
// classes default to not-retryable (conservative) until their signals are wired.
func (r RetryClass) Retryable() bool {
	switch r {
	case RetryCapacity, RetrySpotInterruption:
		return true
	default:
		return false
	}
}

// ClassifyLaunchError maps a verbatim AWS RunInstances error code (as carried on
// aws.LaunchError.Code) to a RetryClass. It takes the code as a plain string so
// this package stays free of the AWS SDK — the caller extracts .Code and passes
// it in. An unrecognized or empty code yields RetryNone (treated as terminal).
//
// This deliberately re-derives, rather than imports, the capacity taxonomy in
// lagotto/pkg/failure — importing lagotto from spawn would invert the dependency
// direction. Keep the two in rough sync when adding codes.
func ClassifyLaunchError(code string) RetryClass {
	switch code {
	case "InsufficientInstanceCapacity",
		"InsufficientHostCapacity",
		"InsufficientReservedInstanceCapacity",
		"Unsupported", // often "no capacity for this type in this AZ"
		"MaxSpotInstanceCountExceeded",
		"SpotMaxPriceTooLow",
		"RequestLimitExceeded":
		return RetryCapacity
	default:
		return RetryNone
	}
}
