// Package taskproto defines the shared task-execution contract that spore.host
// workflow adapters (nf-spawn, miniwdl-spawn, cwl-spawn, snakemake, airflow)
// target instead of each reimplementing sizing / staging / launch / completion.
//
// This is the first increment (spawn#386): the wire types, spec validation, and
// a ResourceRequest→instance sizer. Real launch and the durable .exitcode-in-S3
// completion protocol are later increments — see docs/workflow-adapter-protocol-rfc.md.
package taskproto

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// TaskSpec is one unit of work to run on one ephemeral instance.
type TaskSpec struct {
	TaskID    string            `json:"task_id"`
	Command   []string          `json:"command"`
	Container string            `json:"container,omitempty"`
	Resources ResourceRequest   `json:"resources"`
	Inputs    []Manifest        `json:"inputs,omitempty"`
	Outputs   []Manifest        `json:"outputs,omitempty"`
	Placement Placement         `json:"placement,omitempty"` // optional launch-time knobs (AMI/AZ/volumes/FSx/EFS)
	Lifecycle Lifecycle         `json:"lifecycle"`
	Env       map[string]string `json:"env,omitempty"`
}

// ResourceRequest is what the sizer maps to an instance type.
type ResourceRequest struct {
	CPU                   int      `json:"cpu,omitempty"`
	MemoryGiB             float64  `json:"memory_gib,omitempty"`
	GPUs                  int      `json:"gpus,omitempty"`
	Architecture          string   `json:"architecture,omitempty"` // x86_64 | arm64 | "" (any)
	Families              []string `json:"families,omitempty"`     // allow-list of family prefixes (e.g. c7i, m7i)
	Purchase              string   `json:"purchase,omitempty"`     // spot | on_demand (default on_demand)
	Fallback              string   `json:"fallback,omitempty"`     // on_demand when spot unavailable
	MemoryHeadroomPercent int      `json:"memory_headroom_percent,omitempty"`
	// S3ReadWrite lists s3://bucket[/prefix] URIs the task needs FULL scoped access
	// to (ListBucket + Get/Put/Delete on the whole bucket), beyond the buckets
	// implied by Inputs/Outputs manifests. For adapters whose own tooling does the
	// S3 I/O rather than the spawn wrapper — e.g. Snakemake's S3 storage plugin,
	// which lists/reads/writes/deletes across its storage bucket. Bucket-scoped:
	// the prefix (if any) is advisory; the grant is on the whole bucket because the
	// plugin does bucket-level ListBucket.
	S3ReadWrite []string `json:"s3_read_write,omitempty"`
}

// Placement carries optional launch-time knobs an adapter needs beyond sizing:
// a specific AMI/AZ, attached reference-data volumes, and shared filesystems.
// These map onto the corresponding spawn launch parameters. All optional — an
// empty Placement launches with the auto-detected AMI, EC2-chosen AZ, and no
// extra storage (today's behavior).
type Placement struct {
	AMI              string      `json:"ami,omitempty"`               // AMI id (ami-...); empty = auto-detect latest AL2023 for the arch
	AvailabilityZone string      `json:"availability_zone,omitempty"` // pin an AZ; empty = EC2 chooses
	Volumes          []VolumeRef `json:"volumes,omitempty"`           // EBS volumes from snapshots, mounted at a path
	FSxLustreID      string      `json:"fsx_lustre_id,omitempty"`     // existing FSx for Lustre filesystem id (fs-...) to mount
	EFSID            string      `json:"efs_id,omitempty"`            // existing EFS filesystem id (fs-...) to mount
}

// VolumeRef is one EBS volume to attach from a snapshot, mounted read-only (the
// common shared-reference-data case) or read-write.
type VolumeRef struct {
	Snapshot  string `json:"snapshot"`            // snap-... to create the volume from
	MountPath string `json:"mount_path"`          // where to mount it on the instance
	ReadOnly  bool   `json:"read_only,omitempty"` // mount read-only (default false = read-write)
}

// Manifest is one input or output staging entry (source → destination).
type Manifest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// Lifecycle controls how the launched instance ends.
type Lifecycle struct {
	TTL        string `json:"ttl"`         // e.g. "4h"; the hard deadline
	OnComplete string `json:"on_complete"` // terminate | stop | hibernate
}

// TaskState is the coarse execution state of a task.
type TaskState string

const (
	StateSubmitted TaskState = "submitted"
	StateLaunching TaskState = "launching"
	StateRunning   TaskState = "running"
	StateCompleted TaskState = "completed"
	StateFailed    TaskState = "failed"
	StateCancelled TaskState = "cancelled"
)

// Purchase modes and on_complete actions.
const (
	PurchaseOnDemand = "on_demand"
	PurchaseSpot     = "spot"
)

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validEnvKey reports whether k is a valid shell identifier usable as an
// environment-variable name in the generated wrapper's `export`.
func validEnvKey(k string) bool { return envKeyRe.MatchString(k) }

var validOnComplete = map[string]bool{"terminate": true, "stop": true, "hibernate": true}
var validPurchase = map[string]bool{"": true, PurchaseOnDemand: true, PurchaseSpot: true}
var validArch = map[string]bool{"": true, "x86_64": true, "arm64": true}

// ParseSpecFile reads and parses a TaskSpec from a JSON file, then validates it.
func ParseSpecFile(path string) (*TaskSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read task spec %s: %w", path, err)
	}
	var spec TaskSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse task spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// Validate performs static checks on a TaskSpec: required fields, known enum
// values, and a resolvable TTL. It does not contact AWS.
func (s *TaskSpec) Validate() error {
	var probs []string
	add := func(f string, a ...interface{}) { probs = append(probs, fmt.Sprintf(f, a...)) }

	if strings.TrimSpace(s.TaskID) == "" {
		add("task_id is required")
	}
	if len(s.Command) == 0 {
		add("command is required (argv, non-empty)")
	}
	if !validArch[s.Resources.Architecture] {
		add("resources.architecture %q invalid (want x86_64, arm64, or empty)", s.Resources.Architecture)
	}
	if !validPurchase[s.Resources.Purchase] {
		add("resources.purchase %q invalid (want spot or on_demand)", s.Resources.Purchase)
	}
	if s.Resources.CPU < 0 || s.Resources.MemoryGiB < 0 || s.Resources.GPUs < 0 {
		add("resources cpu/memory_gib/gpus must be non-negative")
	}
	if s.Resources.MemoryHeadroomPercent < 0 || s.Resources.MemoryHeadroomPercent > 100 {
		add("resources.memory_headroom_percent must be 0-100")
	}
	if strings.TrimSpace(s.Lifecycle.TTL) == "" {
		add("lifecycle.ttl is required (e.g. 4h)")
	} else if _, err := time.ParseDuration(s.Lifecycle.TTL); err != nil {
		add("lifecycle.ttl %q is not a valid duration", s.Lifecycle.TTL)
	}
	if s.Lifecycle.OnComplete != "" && !validOnComplete[s.Lifecycle.OnComplete] {
		add("lifecycle.on_complete %q invalid (want terminate, stop, or hibernate)", s.Lifecycle.OnComplete)
	}
	for i, m := range s.Inputs {
		if m.Source == "" || m.Destination == "" {
			add("inputs[%d]: source and destination are both required", i)
		}
	}
	for i, m := range s.Outputs {
		if m.Source == "" || m.Destination == "" {
			add("outputs[%d]: source and destination are both required", i)
		}
	}
	for i, u := range s.Resources.S3ReadWrite {
		if !strings.HasPrefix(u, "s3://") {
			add("resources.s3_read_write[%d] %q must be an s3:// URI", i, u)
		}
	}
	for i, v := range s.Placement.Volumes {
		if !strings.HasPrefix(v.Snapshot, "snap-") {
			add("placement.volumes[%d].snapshot %q must be a snap- id", i, v.Snapshot)
		}
		if strings.TrimSpace(v.MountPath) == "" {
			add("placement.volumes[%d].mount_path is required", i)
		}
	}
	// Env keys are exported verbatim (unquoted) in the generated wrapper, so they
	// must be valid shell identifiers; values are single-quoted and unrestricted.
	for k := range s.Env {
		if !validEnvKey(k) {
			add("env key %q invalid (want a shell identifier: [A-Za-z_][A-Za-z0-9_]*)", k)
		}
	}
	if len(probs) > 0 {
		return fmt.Errorf("invalid task spec:\n  - %s", strings.Join(probs, "\n  - "))
	}
	return nil
}

// EffectiveOnComplete returns the configured on_complete or the safe default
// (terminate) — an ephemeral task should never outlive its work.
func (s *TaskSpec) EffectiveOnComplete() string {
	if s.Lifecycle.OnComplete == "" {
		return "terminate"
	}
	return s.Lifecycle.OnComplete
}
