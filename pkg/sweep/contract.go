package sweep

import (
	"encoding/json"
	"fmt"
	"time"
)

// The contract between whoever starts a detached sweep and
// spawn-sweep-orchestrator.
//
// The orchestrator is invoked with an OrchestratorEvent carrying nothing but a
// sweep ID. Everything else it needs it loads from the SweepRecord in
// spawn-sweep-orchestration, and it then downloads the params from the s3:// URI
// in that record's S3ParamsKey and json-decodes them. So a caller must, in
// order: stage the params as JSON, write the record, invoke.
//
// Two callers exist — `spawn launch --param-file` (CLI) and scheduler-handler
// (Lambda) — and the second had only ever done the last step: it sent a sweep ID
// for which no record existed, so every scheduled sweep failed at
// loadSweepState (#1026). Both now build what they send from this file, and
// lambda/sweep-orchestrator's contract test consumes it from the other side.

// OrchestratorEvent is the payload spawn-sweep-orchestrator is invoked with.
type OrchestratorEvent struct {
	SweepID       string `json:"sweep_id"`
	ForceDownload bool   `json:"force_download"`
}

// SweepsBucket is the bucket the orchestrator reads params from in region. Its
// IAM role is granted spawn-sweeps-<region>/sweeps/* and nothing else, which is
// why a scheduled sweep's params are re-staged here rather than pointed at in
// the schedules bucket.
func SweepsBucket(region string) string {
	return getS3BucketName(nil, region)
}

// ParamsObjectKey is the object key of a sweep's staged params.
func ParamsObjectKey(sweepID string) string {
	return fmt.Sprintf("sweeps/%s/params.json", sweepID)
}

// ParamsURI is the form SweepRecord.S3ParamsKey must take: s3://bucket/key. The
// orchestrator takes the bucket from it; a bare key would have its first path
// segment read as the bucket name.
func ParamsURI(bucket, key string) string {
	return fmt.Sprintf("s3://%s/%s", bucket, key)
}

// EncodeParams is the body staged at ParamsObjectKey. JSON, because the
// orchestrator json-decodes it — a schedule's params are uploaded as the user's
// YAML and must be converted, not copied.
func EncodeParams(p *ParamFileFormat) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("no params")
	}
	return json.Marshal(p)
}

// ScheduledSweep is what scheduler-handler knows when a schedule fires.
type ScheduledSweep struct {
	SweepID    string
	SweepName  string
	UserID     string
	ScheduleID string
	// AccountID is the compute account the sweep's instances launch in; the
	// orchestrator assumes SpawnSweepCrossAccountRole there.
	AccountID     string
	Region        string
	MaxConcurrent int
	LaunchDelay   string
	// ParamsBucket is where the params were staged (SweepsBucket of the
	// orchestrator's region, unless overridden).
	ParamsBucket string
	Params       *ParamFileFormat
}

// NewScheduledSweepRecord builds the record a scheduled sweep is orchestrated
// from: the same shape `spawn launch --param-file` writes via CreateSweepRecord,
// with Source "scheduled".
func NewScheduledSweepRecord(in ScheduledSweep, now time.Time) (*SweepRecord, error) {
	switch {
	case in.SweepID == "":
		return nil, fmt.Errorf("scheduled sweep: no sweep ID")
	case in.AccountID == "":
		return nil, fmt.Errorf("scheduled sweep %s: no target account ID; the schedule predates aws_account_id — recreate it with spawn schedule create", in.SweepID)
	case in.Region == "":
		return nil, fmt.Errorf("scheduled sweep %s: no region", in.SweepID)
	case in.ParamsBucket == "":
		return nil, fmt.Errorf("scheduled sweep %s: no params bucket", in.SweepID)
	case in.Params == nil || len(in.Params.Params) == 0:
		return nil, fmt.Errorf("scheduled sweep %s: no parameter sets", in.SweepID)
	}

	ts := now.UTC().Format(time.RFC3339)
	record := &SweepRecord{
		SweepID:       in.SweepID,
		SweepName:     in.SweepName,
		UserID:        in.UserID,
		CreatedAt:     ts,
		UpdatedAt:     ts,
		S3ParamsKey:   ParamsURI(in.ParamsBucket, ParamsObjectKey(in.SweepID)),
		MaxConcurrent: in.MaxConcurrent,
		LaunchDelay:   in.LaunchDelay,
		TotalParams:   len(in.Params.Params),
		Region:        in.Region,
		AWSAccountID:  in.AccountID,
		Status:        "INITIALIZING",
		Instances:     []SweepInstance{},
		Source:        "scheduled",
		ScheduleID:    in.ScheduleID,
	}

	// Rows without a region of their own go to the schedule's region, not to
	// GroupParamsByRegion's us-east-1 fallback.
	defaults := map[string]interface{}{}
	for k, v := range in.Params.Defaults {
		defaults[k] = v
	}
	if r, _ := defaults["region"].(string); r == "" {
		defaults["region"] = in.Region
	}
	groups := GroupParamsByRegion(in.Params.Params, defaults)
	if len(groups) == 1 {
		for r := range groups {
			record.Region = r
		}
	} else if len(groups) > 1 {
		record.MultiRegion = true
		record.RegionStatus = make(map[string]*RegionProgress, len(groups))
		for r, indices := range groups {
			record.RegionStatus[r] = &RegionProgress{NextToLaunch: indices}
		}
	}
	return record, nil
}
