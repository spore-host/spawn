// Package taskproto defines the shared task-execution contract that spore.host
// workflow adapters (nf-spawn, miniwdl-spawn, cwl-spawn, snakemake, airflow)
// target instead of each reimplementing sizing / staging / launch / completion.
//
// This is the first increment (spawn#386): the wire types, spec validation, and
// a ResourceRequest→instance sizer. Real launch and the durable .exitcode-in-S3
// completion protocol are later increments — see docs/workflow-adapter-protocol-rfc.md.
package taskproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spore-host/spawn/pkg/ttl"
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
	Env       map[string]string `json:"env,omitempty"` // exported in the wrapper shell; a container task also gets each key via docker run -e (#1059)

	// ResultsPrefix overrides where this task's durable records go — an
	// s3://bucket/prefix under which <task_id>/completion.json, /.exitcode and
	// /command.log are written (spawn#646).
	//
	// Empty means s3://spawn-results-<account>-<region>/tasks, which is what every
	// existing adapter polls, so omitting it changes nothing. Set it to collapse a
	// consumer's two reads into one: outputs[].destination was always fully
	// caller-controlled, and this makes the terminal record follow.
	//
	// The bucket must already exist unless it is spawn's own default — spawn will
	// not create a bucket you named, because a typo would leave a stray bucket and
	// still report success.
	ResultsPrefix string `json:"results_prefix,omitempty"`

	// Regenerable opts this task's outputs in to deletion by spawn's retention
	// sweeper (#1076; owner decision 2026-10-17: deletion is opt-in per spec,
	// tiering is automatic). Set it only for a deterministic task: one that
	// produces the same outputs from the same inputs and recipe. spawn can't know
	// that, and deletion is irreversible. Even when set, the sweeper re-verifies
	// at deletion time that every input is unchanged and the recipe is readable.
	Regenerable bool `json:"regenerable,omitempty"`

	// ContainerDockerSocket bind-mounts the host's Docker socket into the
	// container and adds the socket's group, so a workflow engine image
	// (Cromwell's local backend, miniwdl, Nextflow with docker.enabled) can
	// start its own task containers as siblings (#1231). Off by default and
	// opt-in per spec because access to the socket is ROOT-EQUIVALENT on the
	// host: anything that can talk to dockerd can start a privileged container
	// that mounts /. Requires container; refused for a pooled task, where one
	// task could then control the other tasks on its shared worker
	// (ValidatePooled). Recorded in the provenance recipe.
	ContainerDockerSocket bool `json:"container_docker_socket,omitempty"`
}

// ResourceRequest is what the sizer maps to an instance type.
type ResourceRequest struct {
	CPU          int     `json:"cpu,omitempty"`
	MemoryGiB    float64 `json:"memory_gib,omitempty"`
	GPUs         int     `json:"gpus,omitempty"`
	Architecture string  `json:"architecture,omitempty"` // x86_64 | arm64 | "" (any)
	// InstanceType, when set, pins the EXACT instance type — the sizer returns it
	// verbatim and skips family/price selection. For adapters that expose a
	// specific type (e.g. nf-spawn's ext.instanceType) rather than a
	// cpu/memory request. Takes precedence over Families/CPU/MemoryGiB.
	InstanceType          string   `json:"instance_type,omitempty"`
	Families              []string `json:"families,omitempty"` // allow-list of family prefixes (e.g. c7i, m7i)
	Purchase              string   `json:"purchase,omitempty"` // spot | on_demand (default on_demand)
	Fallback              string   `json:"fallback,omitempty"` // on_demand when spot unavailable
	MemoryHeadroomPercent int      `json:"memory_headroom_percent,omitempty"`
	// DiskGiB requests a root EBS volume of at least this size, mapping onto
	// spawn launch's --volume-size (aws.LaunchConfig.RootVolumeSizeGiB). Zero
	// (the default) means "unset" — the AMI default root size applies, same as
	// omitting --volume-size on the regular launch path. Placement.Volumes
	// attaches separate reference-data volumes; DiskGiB is the root disk a task's
	// staged inputs/outputs and working directory live on (#556).
	DiskGiB int32 `json:"disk_gib,omitempty"`
	// ShmGiB sizes a container task's /dev/shm, mapping onto `docker run
	// --shm-size=<n>g` (#1111). Unset (nil) keeps Docker's default of 64 MB,
	// which is too small for many-rank MPI on one box: Open MPI's shared-memory
	// segments for 64 ranks do not fit, and the task dies with SIGBUS in its
	// first collective. PyTorch DataLoader workers, MPICH and Ray hit the same
	// ceiling.
	//
	// A pointer so that an explicit 0 is distinguishable from "not set" and can
	// be rejected. When set it must be positive, the spec must have a container
	// (the host path has no 64 MB limit to raise), and it may not exceed the
	// instance's memory: for a sized spec, the effective memory request
	// (memory_gib with headroom, or the container memory floor when that is
	// larger); for a pinned instance_type, the type's memory, checked at launch.
	// /dev/shm is RAM-backed, so its pages count against the same memory the
	// workload uses.
	ShmGiB *float64 `json:"shm_gib,omitempty"`
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
	// SubnetID launches the task into this subnet, and so into its VPC
	// (spawn#1267). Empty uses the region's default VPC, which accounts built by
	// Control Tower or Landing Zone Accelerator usually do not have; `spawn
	// launch --subnet-id` is the same choice for a plain launch. A subnet pins
	// its AZ, so availability_zone, if also set, must be the subnet's.
	SubnetID string `json:"subnet_id,omitempty"`
	// EFSMountPoint/FSxMountPoint override the default mount path for EFSID/
	// FSxLustreID (spawn#570 sub-issue 3). Empty means the default ("/efs",
	// "/fsx") — use DefaultEFSMountPoint/DefaultFSxMountPoint or the
	// Effective*MountPoint helpers below rather than the raw field, so every
	// caller (the boot-time storage script in pkg/tasklaunch AND the
	// container-mount list in wrapper.go) resolves the SAME path for a given
	// spec instead of each hardcoding "/efs"/"/fsx" independently.
	EFSMountPoint string `json:"efs_mount_point,omitempty"`
	FSxMountPoint string `json:"fsx_mount_point,omitempty"`
}

// DefaultEFSMountPoint/DefaultFSxMountPoint are the mount paths used when a
// Placement doesn't override them. Exported as named constants (rather than
// each caller repeating the literal "/efs"/"/fsx") so pkg/tasklaunch's boot-time
// mount script and wrapper.go's container `docker run -v` list can never
// silently drift onto different defaults (spawn#570).
const (
	DefaultEFSMountPoint = "/efs"
	DefaultFSxMountPoint = "/fsx"
)

// EffectiveEFSMountPoint returns p.EFSMountPoint, or DefaultEFSMountPoint if
// unset. Meaningless when p.EFSID == "" (no EFS in this placement).
func (p Placement) EffectiveEFSMountPoint() string {
	if p.EFSMountPoint != "" {
		return p.EFSMountPoint
	}
	return DefaultEFSMountPoint
}

// EffectiveFSxMountPoint returns p.FSxMountPoint, or DefaultFSxMountPoint if
// unset. Meaningless when p.FSxLustreID == "" (no FSx in this placement).
func (p Placement) EffectiveFSxMountPoint() string {
	if p.FSxMountPoint != "" {
		return p.FSxMountPoint
	}
	return DefaultFSxMountPoint
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
	// AllowEmpty lets a directory input (a source ending in "/") stage nothing
	// (#1207). Without it, a prefix that is empty or missing fails stage-in as
	// staging_error, because that is usually an upstream task that never wrote
	// its output. Valid only on a directory input.
	AllowEmpty bool `json:"allow_empty,omitempty"`
}

// Lifecycle controls how the launched instance ends.
type Lifecycle struct {
	TTL        string `json:"ttl"`         // e.g. "4h"; the hard deadline
	OnComplete string `json:"on_complete"` // terminate | stop | hibernate
	// CostLimit maps onto spawn launch's --cost-limit: terminate/stop when
	// compute spend reaches this amount in USD (compute cost only). Zero (the
	// default) means disabled, matching --cost-limit's own default. TTL and
	// CostLimit are independent spend bounds, both enforced by the same
	// launched-instance mechanism (aws.LaunchConfig.CostLimit / spored's
	// cap-check) — set here so a task's cost ceiling doesn't have to be
	// expressed indirectly by shortening the TTL (#556).
	//
	// The kill is not instant: the terminal flush, which stages the declared
	// outputs for up to FlushOutputsSeconds, runs before the instance stops,
	// and those seconds are billed. So a cost_limit kill can overshoot the
	// limit by at most FlushOutputsSeconds + 5 s of compute (spawn#1297).
	CostLimit float64 `json:"cost_limit,omitempty"`

	// FlushOutputsSeconds bounds how long a lifecycle kill (ttl, cost limit,
	// idle, Spot interruption) spends staging the task's declared outputs
	// before the instance goes (spawn#1297). The terminal-flush hook uploads
	// command.log, then the outputs within this budget, then the completion
	// record, which states what happened in outputs_staged. Unset means
	// DefaultFlushOutputsSeconds; 0 disables it; at most
	// MaxFlushOutputsSeconds. On a Spot interruption spored grants less, so
	// the whole flush fits the 2-minute notice. The seconds are billed: after
	// a cost_limit kill they are a bounded overshoot of the limit.
	FlushOutputsSeconds *int `json:"flush_outputs_seconds,omitempty"`
}

// The bounds of Lifecycle.FlushOutputsSeconds (spawn#1297). 60 s stages a
// task's logs and counters, the usual diagnostic outputs, without making a
// kill expensive; 300 s caps what a spec can add to every kill.
const (
	DefaultFlushOutputsSeconds = 60
	MaxFlushOutputsSeconds     = 300
)

// FlushOutputsBudget is the effective output budget of a lifecycle kill, in
// seconds: FlushOutputsSeconds, or DefaultFlushOutputsSeconds when unset,
// clamped to 0..MaxFlushOutputsSeconds.
func (l Lifecycle) FlushOutputsBudget() int {
	if l.FlushOutputsSeconds == nil {
		return DefaultFlushOutputsSeconds
	}
	return min(max(*l.FlushOutputsSeconds, 0), MaxFlushOutputsSeconds)
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
	return ParseSpec(data)
}

// ParseSpec parses a TaskSpec from raw JSON bytes, then validates it. Used by the
// pooled-worker path, which fetches a staged spec from S3 rather than a local file.
//
// Parsing is strict (json.Decoder.DisallowUnknownFields): a spec is authored once
// and machine-consumed forever, so a misremembered field name (e.g. "cpus" instead
// of "cpu", or a "disk_gb" that doesn't exist) must be a parse error naming the bad
// field — not silently discarded, which used to leave Validate() checking only the
// fields that happened to parse and produce a plausible-looking but wrong (usually
// much smaller) launch plan (#556).
func ParseSpec(data []byte) (*TaskSpec, error) {
	var spec TaskSpec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
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
	if s.Resources.DiskGiB < 0 {
		add("resources.disk_gib must be non-negative")
	}
	if msg := s.shmProblem(); msg != "" {
		add("%s", msg)
	}
	if s.ContainerDockerSocket && s.Container == "" {
		add("container_docker_socket applies only to a container task (set container, or remove container_docker_socket)")
	}
	if s.Lifecycle.CostLimit < 0 {
		add("lifecycle.cost_limit must be non-negative")
	}
	if f := s.Lifecycle.FlushOutputsSeconds; f != nil && (*f < 0 || *f > MaxFlushOutputsSeconds) {
		add("lifecycle.flush_outputs_seconds must be 0-%d (0 disables staging outputs on a lifecycle kill)", MaxFlushOutputsSeconds)
	}
	if strings.TrimSpace(s.Lifecycle.TTL) == "" {
		add("lifecycle.ttl is required (e.g. 4h)")
	} else if _, err := ttl.ParseTTL(s.Lifecycle.TTL); err != nil {
		// The shared parser spored and the TTL reaper use (#990): Go syntax
		// plus days ("1d", "2d12h"), and positive.
		add("lifecycle.ttl %q is not a valid duration (e.g. 4h, 90m, 1d): %v", s.Lifecycle.TTL, err)
	}
	for _, u := range StagingBucketUses(s) {
		add("%s names a spawn staging bucket (%s*), which holds other tasks' staged wrappers; no task may read, write or list it (spawn#1274)", u, StagingBucketPrefix)
	}
	if s.Lifecycle.OnComplete != "" && !validOnComplete[s.Lifecycle.OnComplete] {
		add("lifecycle.on_complete %q invalid (want terminate, stop, or hibernate)", s.Lifecycle.OnComplete)
	}
	// The wrapper unconditionally emits `aws s3 cp <Source> <Destination>` for
	// every input and `aws s3 cp <Source> <Destination>` for every output
	// (wrapper.go's stage-in/stage-out loops) — there is no scheme dispatch, so
	// whichever side is meant to be the S3 side MUST actually be an s3:// URI or
	// the `aws s3 cp` call fails at runtime with no earlier warning (spawn#570
	// sub-issue 2). An input's Source is the S3 side (S3 → local Destination); an
	// output's Destination is the S3 side (local Source → S3 Destination).
	// Rejecting this here, the same way resources.s3_read_write is checked
	// below, turns a paid-boot runtime failure into an immediate parse/validate
	// error. A manifest entry that wants to reference a path already mounted on
	// a placement filesystem (e.g. "/efs/refs/GRCh38.fa") isn't supported by
	// this cp-only wrapper today — Placement's EFS/FSx/volume mount already
	// makes that path directly readable/writable by the command or container
	// without going through a manifest entry at all.
	for i, m := range s.Inputs {
		if m.Source == "" || m.Destination == "" {
			add("inputs[%d]: source and destination are both required", i)
		} else if !strings.HasPrefix(m.Source, "s3://") {
			add("inputs[%d].source %q must be an s3:// URI (stage-in only supports S3 → local; a path on a mounted placement filesystem is already directly accessible without a manifest entry)", i, m.Source)
		} else if m.AllowEmpty && recursiveFlag(m.Source) == "" {
			add("inputs[%d].allow_empty applies only to a directory input (a source ending in \"/\"); %q is a single object, which always fails stage-in when missing", i, m.Source)
		}
	}
	for i, m := range s.Outputs {
		if m.Source == "" || m.Destination == "" {
			add("outputs[%d]: source and destination are both required", i)
		} else if !strings.HasPrefix(m.Destination, "s3://") {
			add("outputs[%d].destination %q must be an s3:// URI (stage-out only supports local → S3; a path on a mounted placement filesystem is already directly accessible without a manifest entry)", i, m.Destination)
		}
		if m.AllowEmpty {
			add("outputs[%d].allow_empty is not supported (it applies only to directory inputs)", i)
		}
	}
	for i, u := range s.Resources.S3ReadWrite {
		if !strings.HasPrefix(u, "s3://") {
			add("resources.s3_read_write[%d] %q must be an s3:// URI", i, u)
		}
	}
	if id := s.Placement.SubnetID; id != "" && !strings.HasPrefix(id, "subnet-") {
		add("placement.subnet_id %q must be a subnet- id", id)
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

// ValidatePooled is Validate plus the restrictions of a POOLED task, which runs
// on a worker shared with other tasks (#70) rather than on an instance of its
// own. Every pooled entry point checks it: submission (taskpool.Pool.Enqueue),
// the worker before it runs a staged spec, and GeneratePooledJobScript.
//
// container_docker_socket is refused (#1231): the socket is root-equivalent on
// the worker, so one task could read, alter or kill the other tasks running
// there, and their staged inputs and outputs.
func (s *TaskSpec) ValidatePooled() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if msg := s.pooledProblem(); msg != "" {
		return fmt.Errorf("invalid task spec:\n  - %s", msg)
	}
	return nil
}

// pooledProblem returns why s can't run on a shared pool worker, or "".
// GeneratePooledJobScript checks it too, so no pooled job script can carry
// what a pool refuses.
func (s *TaskSpec) pooledProblem() string {
	if s.ContainerDockerSocket {
		return "container_docker_socket is not allowed for a pooled task: the Docker socket is root-equivalent on the shared worker, so this task could control the other tasks running there; run it with 'spawn task run' on its own instance instead"
	}
	return ""
}

// EffectiveOnComplete returns the configured on_complete or the safe default
// (terminate) — an ephemeral task should never outlive its work.
func (s *TaskSpec) EffectiveOnComplete() string {
	if s.Lifecycle.OnComplete == "" {
		return "terminate"
	}
	return s.Lifecycle.OnComplete
}

// shmProblem returns why resources.shm_gib is invalid, or "" (#1111). The
// memory bound for a pinned instance_type cannot be checked offline; see
// CheckShmAgainstMemory.
func (s *TaskSpec) shmProblem() string {
	shm := s.Resources.ShmGiB
	if shm == nil {
		return ""
	}
	if math.IsNaN(*shm) || math.IsInf(*shm, 0) || *shm <= 0 {
		return fmt.Sprintf("resources.shm_gib %v must be a positive number of GiB (omit it to keep Docker's 64 MB default)", *shm)
	}
	if s.Container == "" {
		return "resources.shm_gib applies only to a container task (set container, or remove shm_gib)"
	}
	if strings.TrimSpace(s.Resources.InstanceType) != "" {
		return ""
	}
	return s.CheckShmAgainstMemory(EffectiveMemoryGiB(s.SizingRequest()))
}

// CheckShmAgainstMemory returns a non-empty message when resources.shm_gib
// exceeds memGiB, the memory of the instance the task will run on (#1111).
// Validate applies it to a sized spec using the effective memory request;
// callers apply it to a pinned resources.instance_type once they know that
// type's memory. Returns "" when shm_gib is unset or memGiB is unknown (<= 0).
func (s *TaskSpec) CheckShmAgainstMemory(memGiB float64) string {
	shm := s.Resources.ShmGiB
	if shm == nil || memGiB <= 0 || *shm <= memGiB {
		return ""
	}
	return fmt.Sprintf("resources.shm_gib %v exceeds the instance's %v GiB of memory (/dev/shm is RAM-backed; raise memory_gib or lower shm_gib)",
		*shm, strconv.FormatFloat(memGiB, 'f', -1, 64))
}

// shmSizeFlag is the `docker run` flag for resources.shm_gib, or "" when it is
// unset (#1111). 'f' formatting never produces an exponent, which Docker's size
// parser would reject.
func shmSizeFlag(r ResourceRequest) string {
	if r.ShmGiB == nil {
		return ""
	}
	return "--shm-size=" + strconv.FormatFloat(*r.ShmGiB, 'f', -1, 64) + "g"
}
