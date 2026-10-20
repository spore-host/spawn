package tasklaunch

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/ecrref"
	"github.com/spore-host/spawn/pkg/taskproto"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

// TaskPlan is what launching a TaskSpec would do, decided without creating
// anything: the sized instance, the AMI it would boot, what it may cost and how
// big its user data is. Plan builds it, Render prints it, and Launch acts on it.
type TaskPlan struct {
	Spec   *taskproto.TaskSpec
	Region string
	// Sized is the instance type with its sizes and on-demand rate. A pinned
	// resources.instance_type has Sized.Pinned set; when its lookup failed,
	// Sized.SizesKnown() is false and the sizes are unknown, not zero.
	Sized *taskproto.SizeResult
	// AMI describes the image the launch will boot: the explicit placement.ami,
	// or the auto-selected AL2023 (the NVIDIA DLAMI for a GPU type, spawn#601).
	AMI string
	// Purchase is resources.purchase with its default (on_demand) applied.
	Purchase string
	// MaxCost is the on-demand rate times the TTL, in dollars. MaxCostKnown is
	// false when either is unknown.
	MaxCost      float64
	MaxCostKnown bool
	// UserDataBytes is the size of the user data the launch would send, as
	// EC2's 16 KiB limit counts it (#1063). Plan fails a spec over the limit.
	UserDataBytes int
	// Notes annotate the sizing choice (the container memory floor, an
	// old-generation family). Render prints them under the Instance line.
	Notes []string
	// Warnings are for the user before anything launches, e.g. a pinned
	// container type below the container memory floor (#1180). Render does not
	// print them; Launch writes them to Deps.Warn.
	Warnings []string
}

// Plan sizes spec and checks that it can launch, without creating anything. It
// is what `spawn task run --dry-run` prints, and the first step of Launch.
//
// Sizing uses spec.SizingRequest(), so a container task gets the container
// memory floor (#1180). A pinned resources.instance_type is looked up once
// through deps.Finder (#1253), and the pinned-type checks (the container floor
// warning and the shm_gib bound, #1111) read its memory from that lookup,
// asking deps.AWS only if it failed. The user data is measured before any AWS
// call (#1063), so an oversized spec fails here. deps.Finder is required;
// deps.AWS may be nil.
func Plan(ctx context.Context, deps Deps, spec *taskproto.TaskSpec, region string) (*TaskPlan, error) {
	sized, err := taskproto.Size(ctx, deps.Finder, spec.SizingRequest())
	if err != nil {
		return nil, err
	}
	mem := sizedMemoryLookup{sized: sized}
	if deps.AWS != nil {
		mem.fallback = deps.AWS
	}
	var warning strings.Builder
	warnIfContainerPinBelowFloor(ctx, mem, &warning, region, spec)
	if err := checkPinnedShm(ctx, mem, region, spec); err != nil {
		return nil, err
	}
	// The user data this task would send, measured the way RunInstances
	// measures it, so a spec that cannot launch fails its dry run (#1063).
	userDataBytes, err := taskUserDataPreflight(spec, sized, region)
	if err != nil {
		return nil, err
	}

	p := &TaskPlan{
		Spec:          spec,
		Region:        region,
		Sized:         sized,
		AMI:           taskAMIPlan(spec, sized.InstanceType),
		Purchase:      spec.Resources.Purchase,
		UserDataBytes: userDataBytes,
	}
	if p.Purchase == "" {
		p.Purchase = taskproto.PurchaseOnDemand
	}
	if d, err := ttlpkg.ParseDuration(spec.Lifecycle.TTL); err == nil && sized.OnDemandPrice > 0 {
		p.MaxCost, p.MaxCostKnown = sized.OnDemandPrice*d.Hours(), true
	}
	for _, note := range []string{containerMemoryFloorNote(spec), oldGenerationNote(sized.Family)} {
		if note != "" {
			p.Notes = append(p.Notes, note)
		}
	}
	if w := warning.String(); w != "" {
		p.Warnings = append(p.Warnings, w)
	}
	return p, nil
}

// Render writes the plan as `spawn task run --dry-run` prints it, from the
// "DRY RUN" banner through the inputs and outputs. It prints neither
// p.Warnings nor a closing "how to launch" line, since those differ by front
// end.
func Render(out io.Writer, p *TaskPlan) {
	fmt.Fprintln(out, "DRY RUN — nothing will be launched.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Task:         %s\n", p.Spec.TaskID)
	fmt.Fprintf(out, "Command:      %s\n", strings.Join(p.Spec.Command, " "))
	if p.Spec.Container != "" {
		mode := "container (docker run"
		if p.Spec.Resources.GPUs > 0 {
			mode += ", --gpus all"
		}
		if ecrref.Account(p.Spec.Container) != "" {
			mode += ", private ECR"
		}
		mode += ")"
		fmt.Fprintf(out, "Container:    %s  [%s]\n", p.Spec.Container, mode)
	}
	fmt.Fprintf(out, "Region:       %s\n", p.Region)
	fmt.Fprintf(out, "Instance:     %s  (%s)\n", p.Sized.InstanceType, instanceSizeSummary(p.Sized))
	if p.Sized.Pinned {
		fmt.Fprintf(out, "              pinned by spec (resources.instance_type)\n")
	} else {
		fmt.Fprintf(out, "              chosen as cheapest of %d matching type(s)\n", p.Sized.Considered)
	}
	for _, note := range p.Notes {
		fmt.Fprintf(out, "              %s\n", note)
	}
	// AMI: show what will actually be launched (spawn#601). Without this line an
	// author could not tell a GPU spec would get the NVIDIA DLAMI vs the driverless
	// default AL2023 — the issue's whole confusion. Classified OFFLINE from the
	// sized type (no AWS): the real launch resolves the same choice via
	// launcher.Provision → GetRecommendedAMI, which uses these same classifiers.
	fmt.Fprintf(out, "AMI:          %s\n", p.AMI)
	// Root disk: show what will actually apply, whether that's the TaskSpec's
	// explicit resources.disk_gib or the AMI default — the issue's complaint
	// (#556) was that NOTHING in the preview tipped off an author who forgot to
	// set it and got an 8 GiB root, so the AMI-default case is shown too.
	if p.Spec.Resources.DiskGiB > 0 {
		fmt.Fprintf(out, "Root disk:    %d GiB (resources.disk_gib)\n", p.Spec.Resources.DiskGiB)
	} else {
		fmt.Fprintf(out, "Root disk:    AMI default (resources.disk_gib not set; see --volume-size in 'spawn launch --help')\n")
	}
	if p.Spec.Container != "" {
		if shm := p.Spec.Resources.ShmGiB; shm != nil {
			fmt.Fprintf(out, "/dev/shm:     %s GiB (resources.shm_gib)\n", strconv.FormatFloat(*shm, 'f', -1, 64))
		} else {
			fmt.Fprintf(out, "/dev/shm:     Docker default, 64 MB (resources.shm_gib not set)\n")
		}
	}
	fmt.Fprintf(out, "Purchase:     %s\n", p.Purchase)
	fmt.Fprintf(out, "TTL:          %s   on-complete: %s\n", p.Spec.Lifecycle.TTL, p.Spec.EffectiveOnComplete())
	if p.Spec.Lifecycle.CostLimit > 0 {
		fmt.Fprintf(out, "Cost limit:   $%.2f (lifecycle.cost_limit)\n", p.Spec.Lifecycle.CostLimit)
	}
	fmt.Fprintf(out, "User data:    %d of %d bytes after gzip (EC2 limit)\n", p.UserDataBytes, aws.MaxUserDataBytes)

	if p.MaxCostKnown {
		fmt.Fprintf(out, "Max cost:     ~$%.2f (on-demand rate × TTL; a completed task usually costs far less)\n", p.MaxCost)
	}

	if len(p.Spec.Inputs) > 0 {
		fmt.Fprintf(out, "Inputs:\n")
		for _, m := range p.Spec.Inputs {
			fmt.Fprintf(out, "  %s → %s\n", m.Source, m.Destination)
		}
	}
	if len(p.Spec.Outputs) > 0 {
		fmt.Fprintf(out, "Outputs:\n")
		for _, m := range p.Spec.Outputs {
			fmt.Fprintf(out, "  %s → %s\n", m.Source, m.Destination)
		}
	}
}

// memoryLookup is the one AWS question the pinned-type checks ask; *aws.Client
// satisfies it.
type memoryLookup interface {
	InstanceMemoryMiB(ctx context.Context, region, instanceType string) (int64, bool)
}

// instanceSizeSummary is the parenthesised part of the dry run's Instance line:
// vCPUs, memory and the on-demand rate when known. A pinned type whose lookup
// failed says so rather than printing zeros (#1253); its rate, if one was found
// anyway, is still shown.
func instanceSizeSummary(sized *taskproto.SizeResult) string {
	var parts []string
	if sized.SizesKnown() {
		parts = append(parts, fmt.Sprintf("%d vCPU", sized.VCPUs), fmt.Sprintf("%.0f GiB", sized.MemoryGiB))
	} else {
		parts = append(parts, fmt.Sprintf("sizes unavailable: %v", sized.PinLookupErr))
	}
	if sized.OnDemandPrice > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f/hr on-demand", sized.OnDemandPrice))
	}
	return strings.Join(parts, ", ")
}

// sizedMemoryLookup answers the pin checks' memory question from the sizing
// lookup, so a pinned type is described once (#1253). Only when that lookup
// could not size the type does it ask fallback, whose own static table still
// knows the tiny sizes the container-floor warning is about.
type sizedMemoryLookup struct {
	sized    *taskproto.SizeResult
	fallback memoryLookup
}

func (m sizedMemoryLookup) InstanceMemoryMiB(ctx context.Context, region, instanceType string) (int64, bool) {
	if m.sized != nil && m.sized.InstanceType == instanceType && m.sized.SizesKnown() && m.sized.MemoryGiB > 0 {
		return int64(m.sized.MemoryGiB*1024 + 0.5), true
	}
	if m.fallback == nil {
		return 0, false
	}
	return m.fallback.InstanceMemoryMiB(ctx, region, instanceType)
}

// containerMemoryFloorNote explains, in the dry run, that sizing raised the
// memory request to the container floor (#1180), or returns "" when it did not.
// Without it the plan would show a 2 GiB box for a spec that asked for none and
// leave the author guessing why.
func containerMemoryFloorNote(spec *taskproto.TaskSpec) string {
	sizing := spec.SizingRequest()
	if sizing.MemoryGiB == spec.Resources.MemoryGiB && sizing.MemoryHeadroomPercent == spec.Resources.MemoryHeadroomPercent {
		return ""
	}
	return fmt.Sprintf("memory raised to the %.0f GiB container floor: Docker is installed at boot and is OOM-killed on smaller boxes (#1180)", taskproto.ContainerMemoryFloorGiB)
}

// renderContainerPinFloorWarning returns the warning for a container task whose
// pinned resources.instance_type has less memory than the container floor, or
// "" when there is nothing to say. Pure, so it is tested without AWS.
//
// A pin is honoured as-is (it is how nf-spawn's ext.instanceType reaches spawn),
// so this warns rather than refuses: the author chose the type, and spawn cannot
// prove the image's Docker install will not fit.
func renderContainerPinFloorWarning(spec *taskproto.TaskSpec, memMiB int64) string {
	pin := strings.TrimSpace(spec.Resources.InstanceType)
	if spec.Container == "" || pin == "" || memMiB <= 0 {
		return ""
	}
	gib := float64(memMiB) / 1024
	if gib >= taskproto.ContainerMemoryFloorGiB {
		return ""
	}
	return fmt.Sprintf("\n⚠️  resources.instance_type %s has %.1f GiB of memory, below the %.0f GiB spawn sizes a container task to.\n"+
		"   The task installs Docker at boot, and on a 0.5 GiB box that install is OOM-killed (#1180); the\n"+
		"   record then reports retry_class instance_health. The pin is honoured as given.\n",
		pin, gib, taskproto.ContainerMemoryFloorGiB)
}

// warnIfContainerPinBelowFloor looks up a pinned container task's instance
// memory and prints renderContainerPinFloorWarning. Best-effort: an unresolved
// lookup says nothing rather than guessing.
func warnIfContainerPinBelowFloor(ctx context.Context, client memoryLookup, w io.Writer, region string, spec *taskproto.TaskSpec) {
	pin := strings.TrimSpace(spec.Resources.InstanceType)
	if spec.Container == "" || pin == "" {
		return
	}
	mem, ok := client.InstanceMemoryMiB(ctx, region, pin)
	if !ok {
		return
	}
	if msg := renderContainerPinFloorWarning(spec, mem); msg != "" {
		fmt.Fprint(w, msg)
	}
}

// checkPinnedShm refuses a container task whose resources.shm_gib exceeds the
// memory of its pinned resources.instance_type (#1111). Validate checks a sized
// spec offline; a pin's memory needs a lookup, so it is checked here. An
// unresolved lookup passes rather than guessing.
func checkPinnedShm(ctx context.Context, client memoryLookup, region string, spec *taskproto.TaskSpec) error {
	pin := strings.TrimSpace(spec.Resources.InstanceType)
	if spec.Resources.ShmGiB == nil || pin == "" {
		return nil
	}
	mem, ok := client.InstanceMemoryMiB(ctx, region, pin)
	if !ok || mem <= 0 {
		return nil
	}
	if msg := spec.CheckShmAgainstMemory(float64(mem) / 1024); msg != "" {
		return fmt.Errorf("invalid task spec: %s (resources.instance_type %s)", msg, pin)
	}
	return nil
}

// oldGenerationFamilies notes instance families that are the cheapest match for
// a bare "arm64" (or similarly unconstrained x86_64) resource request but are an
// older, materially-slower-per-vCPU generation — "cheapest that fits" can pick
// them over a modern equivalent that would cost about the same per job (#556).
// Not an error: families is the documented escape hatch (e.g.
// resources.families: ["c8g"]) — this is just a dry-run nudge to make the
// generation visible before launch rather than after.
var oldGenerationFamilies = map[string]string{
	"a1": "Graviton 1 (2018) — likely slower per vCPU than a current Graviton family (e.g. c8g/m8g/r8g); pin resources.families if that matters",
}

// oldGenerationNote returns a dry-run annotation if family is a known older
// generation, or "" otherwise.
func oldGenerationNote(family string) string {
	if note, ok := oldGenerationFamilies[family]; ok {
		return "instance family: " + family + " — " + note
	}
	return ""
}

// taskAMIPlan describes the AMI a task will launch with, for the dry-run preview
// (spawn#601). It mirrors — offline, without an AWS call — the choice the real
// launch makes: an explicit spec/flag AMI wins; otherwise a GPU-family instance
// gets the AL2023 NVIDIA DLAMI (so `docker run --gpus all` finds a driver) and
// everything else gets the standard AL2023 for the type's architecture. It reuses
// the SAME aws.DetectGPUInstance / aws.DetectArchitecture classifiers that
// GetRecommendedAMI (the real resolver) uses, so the preview cannot drift from
// what gets launched.
func taskAMIPlan(spec *taskproto.TaskSpec, instanceType string) string {
	if ami := strings.TrimSpace(spec.Placement.AMI); ami != "" {
		return ami + "  (explicit: --ami / placement.ami)"
	}
	arch := aws.DetectArchitecture(instanceType)
	if aws.DetectGPUInstance(instanceType) {
		return fmt.Sprintf("AL2023 GPU DLAMI — NVIDIA driver, %s (auto-selected for GPU instance %s; spawn#601)", arch, instanceType)
	}
	return fmt.Sprintf("AL2023 default — %s (auto-selected)", arch)
}
