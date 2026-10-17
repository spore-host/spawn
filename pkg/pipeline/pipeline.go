package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/queue"
	"github.com/spore-host/spawn/pkg/ttl"
)

// Pipeline represents a multi-stage workflow configuration
type Pipeline struct {
	PipelineID   string  `json:"pipeline_id"`
	PipelineName string  `json:"pipeline_name"`
	Stages       []Stage `json:"stages"`

	// S3 configuration
	S3Bucket string `json:"s3_bucket"`
	S3Prefix string `json:"s3_prefix"`

	// Deprecated: nothing writes result_s3_bucket. Stage outputs stay in
	// s3_bucket under the run's prefix, which is where `spawn pipeline
	// collect` reads them; the stage instance profile is granted only that
	// prefix. collect used to read this location instead and reported "No
	// results found" for every pipeline that set it (#989). Still parsed,
	// so existing definitions load, and reported by Deprecations.
	ResultS3Bucket string `json:"result_s3_bucket"`
	// Deprecated: see ResultS3Bucket.
	ResultS3Prefix string `json:"result_s3_prefix"`

	// Failure handling
	OnFailure string `json:"on_failure"` // "stop" or "continue"

	// Budget controls
	MaxCostUSD *float64 `json:"max_cost_usd,omitempty"`

	// Notifications
	NotificationEmail string `json:"notification_email,omitempty"`
}

// Stage represents a single stage in the pipeline
type Stage struct {
	StageID       string   `json:"stage_id"`
	InstanceType  string   `json:"instance_type"`
	InstanceCount int      `json:"instance_count"`
	Region        string   `json:"region"`
	AMI           string   `json:"ami,omitempty"`
	Spot          bool     `json:"spot,omitempty"`
	Timeout       string   `json:"timeout,omitempty"`
	Command       string   `json:"command"`
	DependsOn     []string `json:"depends_on"`

	// Data passing
	DataInput  *DataConfig `json:"data_input,omitempty"`
	DataOutput *DataConfig `json:"data_output,omitempty"`

	// Environment variables
	Env map[string]string `json:"env,omitempty"`

	// Advanced networking
	PlacementGroup string `json:"placement_group,omitempty"`
	EFAEnabled     bool   `json:"efa_enabled,omitempty"`
}

// DefaultStageRegion is where a stage with no region runs. The orchestrator
// launches it there, and validation compares regions with it.
const DefaultStageRegion = "us-east-1"

// EffectiveRegion is the region the stage runs in: its region, or
// DefaultStageRegion.
func (s *Stage) EffectiveRegion() string {
	if s.Region == "" {
		return DefaultStageRegion
	}
	return s.Region
}

// DataConfig defines how data is passed between stages
type DataConfig struct {
	Mode string `json:"mode"` // "s3" or "stream"

	// S3 mode
	SourceStage  string   `json:"source_stage,omitempty"`
	SourceStages []string `json:"source_stages,omitempty"`
	DestPath     string   `json:"dest_path,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	Pattern      string   `json:"pattern,omitempty"`

	// Stream mode
	Protocol string `json:"protocol,omitempty"` // "tcp", "grpc", or "zmq"
	Port     int    `json:"port,omitempty"`
}

// PipelineStatus represents the current state of a pipeline
type PipelineStatus string

const (
	StatusInitializing PipelineStatus = "INITIALIZING"
	StatusRunning      PipelineStatus = "RUNNING"
	StatusCompleted    PipelineStatus = "COMPLETED"
	StatusFailed       PipelineStatus = "FAILED"
	StatusCancelled    PipelineStatus = "CANCELLED"
)

// IsTerminal reports whether the pipeline has stopped moving.
//
// Needed by `spawn pipeline launch --wait` (#807), which polls until this is
// true. The switch is exhaustive on purpose and has no default: a status added
// without classifying it here must not silently fall into "keep waiting", which
// is an unbounded hang rather than a visible error.
// TestEveryPipelineStatusIsClassified enforces that.
func (s PipelineStatus) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	case StatusInitializing, StatusRunning:
		return false
	}
	return false
}

// Succeeded reports whether a TERMINAL status represents success. Meaningless
// before IsTerminal is true.
//
// Separate from IsTerminal because `--wait` has to distinguish them: the point of
// waiting in CI is `spawn pipeline launch --wait && ./collect-results.sh`, so a
// FAILED pipeline must exit non-zero rather than merely stop blocking.
func (s PipelineStatus) Succeeded() bool { return s == StatusCompleted }

// StageStatus represents the current state of a stage
type StageStatus string

const (
	StageStatusPending   StageStatus = "pending"
	StageStatusReady     StageStatus = "ready"
	StageStatusLaunching StageStatus = "launching"
	StageStatusRunning   StageStatus = "running"
	StageStatusCompleted StageStatus = "completed"
	StageStatusFailed    StageStatus = "failed"
	StageStatusSkipped   StageStatus = "skipped"
)

// InstanceInfo tracks information about a launched instance
type InstanceInfo struct {
	InstanceID   string     `json:"instance_id"`
	PrivateIP    string     `json:"private_ip"`
	PublicIP     string     `json:"public_ip"`
	DNSName      string     `json:"dns_name"`
	State        string     `json:"state"`
	LaunchedAt   time.Time  `json:"launched_at"`
	TerminatedAt *time.Time `json:"terminated_at,omitempty"`
}

// StageState tracks runtime state of a stage
type StageState struct {
	StageID       string         `json:"stage_id"`
	StageIndex    int            `json:"stage_index"`
	Status        StageStatus    `json:"status"`
	LaunchedAt    *time.Time     `json:"launched_at,omitempty"`
	CompletedAt   *time.Time     `json:"completed_at,omitempty"`
	ErrorMessage  string         `json:"error_message,omitempty"`
	Instances     []InstanceInfo `json:"instances,omitempty"`
	InstanceHours float64        `json:"instance_hours"`
	StageCostUSD  float64        `json:"stage_cost_usd"`

	// PricePerHourUSD is the on-demand rate of one of the stage's instances,
	// resolved once, and PriceSource where it came from ("live", "static", or
	// "" when unknown). The stage's cost is an ESTIMATE at this rate: spot
	// stages are billed less, and EBS and data transfer are not counted. It
	// was a flat $0.10/hour whatever the type, so max_cost_usd let through
	// about 300x its value on a p4d.24xlarge (#930).
	PricePerHourUSD float64 `json:"price_per_hour_usd,omitempty"`
	PriceSource     string  `json:"price_source,omitempty"`
}

// PipelineState tracks runtime state of entire pipeline
type PipelineState struct {
	PipelineID      string         `json:"pipeline_id" dynamodbav:"pipeline_id"`
	PipelineName    string         `json:"pipeline_name" dynamodbav:"pipeline_name"`
	UserID          string         `json:"user_id" dynamodbav:"user_id"`
	CreatedAt       time.Time      `json:"created_at" dynamodbav:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at" dynamodbav:"updated_at"`
	CompletedAt     *time.Time     `json:"completed_at,omitempty" dynamodbav:"completed_at,omitempty"`
	Status          PipelineStatus `json:"status" dynamodbav:"status"`
	CancelRequested bool           `json:"cancel_requested" dynamodbav:"cancel_requested"`

	// RunID identifies this run of the pipeline. pipeline_id comes from the
	// definition file, so launching the same file twice reuses it; the run
	// id does not repeat. It scopes the run's S3 keys (RunKeyPrefix), its
	// network group names, and the orchestrator's writes: an orchestrator
	// chain whose run id no longer matches the record has been superseded
	// and stops (#943). Empty on records written before it existed.
	RunID string `json:"run_id,omitempty" dynamodbav:"run_id,omitempty"`

	// Configuration
	S3ConfigKey       string   `json:"s3_config_key" dynamodbav:"s3_config_key"`
	S3Bucket          string   `json:"s3_bucket" dynamodbav:"s3_bucket"`
	S3Prefix          string   `json:"s3_prefix" dynamodbav:"s3_prefix"`
	ResultS3Bucket    string   `json:"result_s3_bucket" dynamodbav:"result_s3_bucket"`
	ResultS3Prefix    string   `json:"result_s3_prefix" dynamodbav:"result_s3_prefix"`
	OnFailure         string   `json:"on_failure" dynamodbav:"on_failure"`
	MaxCostUSD        *float64 `json:"max_cost_usd,omitempty" dynamodbav:"max_cost_usd,omitempty"`
	CurrentCostUSD    float64  `json:"current_cost_usd" dynamodbav:"current_cost_usd"`
	NotificationEmail string   `json:"notification_email,omitempty" dynamodbav:"notification_email,omitempty"`

	// InstanceProfile is the instance profile stage instances are launched
	// with. `spawn pipeline launch` creates it, scoped to the pipeline's S3
	// bucket and prefix (StageDataPolicy), because the shared default spored
	// profile grants no access to pipeline data (#911). Empty on records
	// written before it existed; the orchestrator then falls back to the
	// default profile.
	InstanceProfile string `json:"instance_profile,omitempty" dynamodbav:"instance_profile,omitempty"`

	// Progress tracking
	TotalStages     int `json:"total_stages" dynamodbav:"total_stages"`
	CompletedStages int `json:"completed_stages" dynamodbav:"completed_stages"`
	FailedStages    int `json:"failed_stages" dynamodbav:"failed_stages"`

	// Stage details
	Stages []StageState `json:"stages" dynamodbav:"stages"`

	// Network configuration, per stage region: security groups and placement
	// groups are regional, and a stage launched in eu-west-1 cannot use a
	// group created in the Lambda's region (#929). Keyed by region.
	SecurityGroups  map[string]string `json:"security_groups,omitempty" dynamodbav:"security_groups,omitempty"`
	PlacementGroups map[string]string `json:"placement_groups,omitempty" dynamodbav:"placement_groups,omitempty"`

	// Deprecated: the single groups an orchestrator before #929 created, in
	// the Lambda's own region. Read only for pipelines in flight across the
	// upgrade, and cleaned up in that region.
	SecurityGroupID  string `json:"security_group_id,omitempty" dynamodbav:"security_group_id,omitempty"`
	PlacementGroupID string `json:"placement_group_id,omitempty" dynamodbav:"placement_group_id,omitempty"`
}

// LoadPipelineFromFile loads a pipeline definition from a JSON file
func LoadPipelineFromFile(path string) (*Pipeline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	var p Pipeline
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	return &p, nil
}

// LoadPipelineFromJSON loads a pipeline definition from JSON bytes
func LoadPipelineFromJSON(data []byte) (*Pipeline, error) {
	var p Pipeline
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	return &p, nil
}

// Validate validates the pipeline configuration
// Deprecations returns a warning for each deprecated field the definition
// sets. They are warnings, not validation errors, so a definition that sets
// them still launches.
func (p *Pipeline) Deprecations() []string {
	var out []string
	if p.ResultS3Bucket != "" || p.ResultS3Prefix != "" {
		out = append(out, "result_s3_bucket and result_s3_prefix are deprecated and ignored: "+
			"nothing writes there. Stage outputs stay in s3_bucket under "+
			"<s3_prefix>/runs/<run-id>/, and `spawn pipeline collect` downloads them from there. "+
			"To keep results in a particular bucket, set s3_bucket and s3_prefix to it (#989)")
	}
	return out
}

func (p *Pipeline) Validate() error {
	if p.PipelineID == "" {
		return fmt.Errorf("pipeline_id is required")
	}

	if p.PipelineName == "" {
		return fmt.Errorf("pipeline_name is required")
	}

	if len(p.Stages) == 0 {
		return fmt.Errorf("at least one stage is required")
	}

	// Validate on_failure
	if p.OnFailure != "" && p.OnFailure != "stop" && p.OnFailure != "continue" {
		return fmt.Errorf("on_failure must be 'stop' or 'continue', got '%s'", p.OnFailure)
	}
	if p.OnFailure == "" {
		p.OnFailure = "stop" // Default
	}

	// Validate S3 bucket
	if p.S3Bucket == "" {
		return fmt.Errorf("s3_bucket is required")
	}

	// Build stage ID map for dependency validation
	stageIDs := make(map[string]bool)
	for _, stage := range p.Stages {
		if stage.StageID == "" {
			return fmt.Errorf("stage_id is required for all stages")
		}
		if stageIDs[stage.StageID] {
			return fmt.Errorf("duplicate stage_id: %s", stage.StageID)
		}
		stageIDs[stage.StageID] = true
	}

	// Validate each stage
	for i, stage := range p.Stages {
		if err := stage.Validate(stageIDs); err != nil {
			return fmt.Errorf("stage %d (%s): %w", i, stage.StageID, err)
		}
	}

	// Validate DAG (no cycles)
	if err := p.ValidateDAG(); err != nil {
		return err
	}

	return p.validateStreamRegions()
}

// validateStreamRegions rejects a stream-mode data_input whose source stage is
// in another region (#956). Each region has its own pipeline security group,
// whose ingress admits only that group, the stages' instances are in each
// region's default VPC, and peer discovery publishes private IPs: nothing peers
// the VPCs or opens a public path, so the consumer could never connect. Use s3
// mode across regions, or put both stages in one region.
func (p *Pipeline) validateStreamRegions() error {
	for i := range p.Stages {
		st := &p.Stages[i]
		in := st.DataInput
		if in == nil || in.Mode != "stream" {
			continue
		}
		src := p.GetStage(in.SourceStage)
		if src == nil {
			continue // ValidateInput reports it
		}
		if src.EffectiveRegion() != st.EffectiveRegion() {
			return fmt.Errorf("stage %s: streams from stage %s across regions (%s -> %s), which is not supported: "+
				"each region's stages are in their own VPC and security group, with nothing connecting them. "+
				"Use \"mode\": \"s3\" between regions, or run both stages in one region",
				st.StageID, src.StageID, src.EffectiveRegion(), st.EffectiveRegion())
		}
	}
	return nil
}

// Validate validates a single stage configuration
func (s *Stage) Validate(validStageIDs map[string]bool) error {
	if s.InstanceType == "" {
		return fmt.Errorf("instance_type is required")
	}

	if s.InstanceCount < 1 {
		return fmt.Errorf("instance_count must be >= 1, got %d", s.InstanceCount)
	}

	if s.Command == "" {
		return fmt.Errorf("command is required")
	}

	// Validate dependencies exist
	for _, dep := range s.DependsOn {
		if !validStageIDs[dep] {
			return fmt.Errorf("depends_on references unknown stage: %s", dep)
		}
	}

	// Validate timeout format if provided
	if s.Timeout != "" {
		if _, err := ttl.ParseTTL(s.Timeout); err != nil {
			return fmt.Errorf("invalid timeout format '%s': %w", s.Timeout, err)
		}
	}

	// Validate data input
	if s.DataInput != nil {
		if err := s.DataInput.ValidateInput(validStageIDs); err != nil {
			return fmt.Errorf("data_input: %w", err)
		}
	}

	// Validate data output
	if s.DataOutput != nil {
		if err := s.DataOutput.ValidateOutput(); err != nil {
			return fmt.Errorf("data_output: %w", err)
		}
	}

	return nil
}

// ValidateInput validates data input configuration
func (d *DataConfig) ValidateInput(validStageIDs map[string]bool) error {
	if d.Mode != "s3" && d.Mode != "stream" {
		return fmt.Errorf("mode must be 's3' or 'stream', got '%s'", d.Mode)
	}

	if d.Mode == "s3" {
		// Must have either source_stage or source_stages
		if d.SourceStage == "" && len(d.SourceStages) == 0 {
			return fmt.Errorf("s3 mode requires source_stage or source_stages")
		}

		// Validate source stages exist
		if d.SourceStage != "" && !validStageIDs[d.SourceStage] {
			return fmt.Errorf("source_stage references unknown stage: %s", d.SourceStage)
		}
		for _, stage := range d.SourceStages {
			if !validStageIDs[stage] {
				return fmt.Errorf("source_stages references unknown stage: %s", stage)
			}
		}

		// The pattern is matched against each file's path relative to its
		// source stage's output (see OutputRelPath), so an absolute one can
		// never match: such a stage silently received nothing (#944).
		if d.Pattern != "" {
			if strings.HasPrefix(d.Pattern, "/") {
				return fmt.Errorf("pattern %q is absolute; it is matched against each file's path relative to "+
					"the directory of the source stage's data_output pattern (e.g. \"chunk-1-*\" for files from \"/data/chunks/*\")", d.Pattern)
			}
			if _, err := path.Match(d.Pattern, ""); err != nil {
				return fmt.Errorf("pattern %q: %w", d.Pattern, err)
			}
		}
	}

	if d.Mode == "stream" {
		if d.Protocol != "tcp" && d.Protocol != "grpc" && d.Protocol != "zmq" {
			return fmt.Errorf("stream mode requires protocol 'tcp', 'grpc', or 'zmq', got '%s'", d.Protocol)
		}
		if d.SourceStage == "" {
			return fmt.Errorf("stream mode requires source_stage")
		}
		if !validStageIDs[d.SourceStage] {
			return fmt.Errorf("source_stage references unknown stage: %s", d.SourceStage)
		}
	}

	return nil
}

// ValidateOutput validates data output configuration
func (d *DataConfig) ValidateOutput() error {
	if d.Mode != "s3" && d.Mode != "stream" {
		return fmt.Errorf("mode must be 's3' or 'stream', got '%s'", d.Mode)
	}

	if d.Mode == "s3" {
		if len(d.Paths) == 0 {
			return fmt.Errorf("s3 mode requires at least one path")
		}
	}

	if d.Mode == "stream" {
		if d.Protocol != "tcp" && d.Protocol != "grpc" && d.Protocol != "zmq" {
			return fmt.Errorf("stream mode requires protocol 'tcp', 'grpc', or 'zmq', got '%s'", d.Protocol)
		}
		if d.Port < 1 || d.Port > 65535 {
			return fmt.Errorf("port must be 1-65535, got %d", d.Port)
		}
	}

	return nil
}

// ValidateDAG validates the pipeline DAG for cycles
func (p *Pipeline) ValidateDAG() error {
	// Convert stages to queue.JobConfig format
	jobs := make([]queue.JobConfig, len(p.Stages))
	for i, stage := range p.Stages {
		jobs[i] = queue.JobConfig{
			JobID:     stage.StageID,
			DependsOn: stage.DependsOn,
		}
	}

	// Use Kahn's algorithm to detect cycles
	_, err := queue.TopologicalSort(jobs)
	if err != nil {
		return err
	}

	return nil
}

// GetTopologicalOrder returns stages in topological order
func (p *Pipeline) GetTopologicalOrder() ([]string, error) {
	jobs := make([]queue.JobConfig, len(p.Stages))
	for i, stage := range p.Stages {
		jobs[i] = queue.JobConfig{
			JobID:     stage.StageID,
			DependsOn: stage.DependsOn,
		}
	}

	return queue.TopologicalSort(jobs)
}

// GetStage returns a stage by ID
func (p *Pipeline) GetStage(stageID string) *Stage {
	for i := range p.Stages {
		if p.Stages[i].StageID == stageID {
			return &p.Stages[i]
		}
	}
	return nil
}

// HasStreamingStages returns true if any stage uses streaming mode
func (p *Pipeline) HasStreamingStages() bool {
	for _, stage := range p.Stages {
		if stage.DataInput != nil && stage.DataInput.Mode == "stream" {
			return true
		}
		if stage.DataOutput != nil && stage.DataOutput.Mode == "stream" {
			return true
		}
	}
	return false
}

// HasEFAStages returns true if any stage uses EFA
func (p *Pipeline) HasEFAStages() bool {
	for _, stage := range p.Stages {
		if stage.EFAEnabled {
			return true
		}
	}
	return false
}
