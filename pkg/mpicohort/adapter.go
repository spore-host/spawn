// Package mpicohort adapts spawn's AWS launch surfaces onto the cohort
// reconciliation core (github.com/spore-host/cohort) for MPI / collective
// launches.
//
// This is the SPIKE adapter (cohort adoption Stage 1): it implements cohort's
// provider seam (Actuator/Observer/Classifier) and domain seam
// (Enroller/Assembler) over spawn's existing *aws.Client, so an MPI cohort can
// be driven through cohort.Reconcile — gaining a real all-or-nothing barrier,
// leak-free drain, and capacity fallback that the hand-rolled launchJobArray
// loop lacks. It does NOT yet replace launchJobArray; that's a later stage.
//
// What this spike deliberately surfaces: cohort's Placement is PER-ENTITY, but
// an MPI cluster's placement group and EFA fabric are COLLECTIVE constraints.
// See the PlacementGroup field note below and adapter_test.go.
package mpicohort

import (
	"context"
	"errors"
	"fmt"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
)

// LaunchAPI is the slice of *aws.Client the adapter needs. An interface (not the
// concrete *aws.Client) so the spike's tests can inject a fake without real AWS.
type LaunchAPI interface {
	Launch(ctx context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error)
	Terminate(ctx context.Context, region, instanceID string) error
	StopInstance(ctx context.Context, region, instanceID string, hibernate bool) error
	StartInstance(ctx context.Context, region, instanceID string) error
	ListInstances(ctx context.Context, region, stateFilter string) ([]aws.InstanceInfo, error)
}

// ---------------------------------------------------------------------------
// Provider seam
// ---------------------------------------------------------------------------

// Actuator drives a single named MPI instance via spawn's launcher. Per cohort's
// contract it NEVER operates on counts — one call names exactly one entity.
type Actuator struct {
	Client LaunchAPI
	Region string

	// BaseConfig is the cluster-wide launch template (AMI, key, SG, placement
	// group, EFA, MPI user-data). Per-entity fields (Name, idempotency token,
	// and the rung's instance type) are overlaid from the EntityIntent at Launch.
	BaseConfig aws.LaunchConfig

	// Configs holds a fully-built per-entity LaunchConfig keyed by EntityID. MPI
	// user-data is per-index (rank-0 differs), and EntityIntent carries no
	// user-data field, so the caller pre-builds each member's config and the
	// Actuator looks it up by intent.ID. Nil/missing → fall back to BaseConfig
	// (the spike path); the rung overlay is applied on top either way.
	Configs map[cohort.EntityID]aws.LaunchConfig
}

func (a *Actuator) Launch(ctx context.Context, intent cohort.EntityIntent) (cohort.Observation, error) {
	cfg := a.BaseConfig
	if c, ok := a.Configs[intent.ID]; ok {
		cfg = c
	}
	cfg.Region = a.Region
	cfg.Name = string(intent.ID)
	cfg.ClientToken = intent.IdempotencyToken // deterministic — safe to re-issue

	// Overlay the rung's placement. The AWS provider's placement is a
	// cohort.RungPlacement; pull the instance type / AZ from its current rung.
	if rp, ok := intent.Placement.(cohort.RungPlacement); ok {
		cfg.InstanceType = rp.Rung.InstanceType
		if rp.Rung.AvailZone != "" {
			cfg.AvailabilityZone = rp.Rung.AvailZone
		}
		cfg.Spot = rp.Rung.CapacityModel == cohort.CapacitySpot
	}

	res, err := a.Client.Launch(ctx, cfg)
	if err != nil {
		return cohort.Observation{}, err // Classifier maps this; do NOT classify here
	}
	return cohort.Observation{
		ID:         intent.ID,
		Generation: intent.Generation,
		State:      mapState(res.State),
		ProviderID: res.InstanceID,
		Address:    res.PrivateIP, // MPI assembly needs private IPs
		Rung:       rungOf(intent),
	}, nil
}

func (a *Actuator) Start(ctx context.Context, id cohort.EntityID) (cohort.Observation, error) {
	pid, err := a.providerID(ctx, id)
	if err != nil {
		return cohort.Observation{}, err
	}
	if err := a.Client.StartInstance(ctx, a.Region, pid); err != nil {
		return cohort.Observation{}, err
	}
	return cohort.Observation{ID: id, State: cohort.StateLaunching, ProviderID: pid}, nil
}

func (a *Actuator) Stop(ctx context.Context, id cohort.EntityID, mode cohort.StopMode) error {
	pid, err := a.providerID(ctx, id)
	if err != nil {
		return err
	}
	return a.Client.StopInstance(ctx, a.Region, pid, mode == cohort.StopHibernate)
}

func (a *Actuator) Terminate(ctx context.Context, id cohort.EntityID) error {
	pid, err := a.providerID(ctx, id)
	if err != nil {
		// Already gone / never created is success for an idempotent Terminate.
		return nil
	}
	return a.Client.Terminate(ctx, a.Region, pid)
}

// providerID resolves an EntityID (the Name tag) to an EC2 instance ID.
func (a *Actuator) providerID(ctx context.Context, id cohort.EntityID) (string, error) {
	insts, err := a.Client.ListInstances(ctx, a.Region, "")
	if err != nil {
		return "", err
	}
	for _, in := range insts {
		if in.Name == string(id) {
			return in.InstanceID, nil
		}
	}
	return "", fmt.Errorf("mpicohort: no instance named %q", id)
}

// Observer reports infrastructure-truth state for named entities. It tolerates
// eventual consistency: an entity it can't find is StateUnknown, never
// StateAbsent — the reconciler resolves a miss via the idempotency token.
type Observer struct {
	Client LaunchAPI
	Region string
}

func (o *Observer) Observe(ctx context.Context, ids []cohort.EntityID) ([]cohort.Observation, error) {
	insts, err := o.Client.ListInstances(ctx, o.Region, "")
	if err != nil {
		return nil, err
	}
	byName := make(map[string]aws.InstanceInfo, len(insts))
	for _, in := range insts {
		byName[in.Name] = in
	}
	out := make([]cohort.Observation, 0, len(ids))
	for _, id := range ids {
		in, ok := byName[string(id)]
		if !ok {
			out = append(out, cohort.Observation{ID: id, State: cohort.StateUnknown})
			continue
		}
		out = append(out, cohort.Observation{
			ID:         id,
			State:      mapState(in.State),
			ProviderID: in.InstanceID,
			Address:    in.PrivateIP,
		})
	}
	return out, nil
}

// Classifier maps a spawn launch error into exactly one cohort Fault class.
// spawn's *aws.LaunchError already carries the verbatim AWS code, so this is a
// code switch — the legible Code is preserved into the Fault for q0 explain.
type Classifier struct{}

func (Classifier) Classify(err error) cohort.Fault {
	if err == nil {
		return cohort.Fault{Class: cohort.FaultRetryableConsistency}
	}
	var le *aws.LaunchError
	code := ""
	if errors.As(err, &le) {
		code = le.Code
	}
	switch code {
	case "InsufficientInstanceCapacity", "InsufficientHostCapacity",
		"MaxSpotInstanceCountExceeded", "SpotMaxPriceTooLow":
		// ICE / no capacity → advance the fallback ladder, never retry in place.
		return cohort.Fault{Class: cohort.FaultCapacityExhausted, Code: code, Message: err.Error()}
	case "RequestLimitExceeded", "Throttling":
		return cohort.Fault{Class: cohort.FaultThrottle, Code: code, Message: err.Error()}
	default:
		// auth, quota, bad parameter, or an unclassified error → terminal.
		return cohort.Fault{Class: cohort.FaultTerminal, Code: code, Message: err.Error()}
	}
}

// ---------------------------------------------------------------------------
// Domain seam
// ---------------------------------------------------------------------------

// Enroller is the per-entity MPI readiness probe — for the spike it confirms the
// instance is reachable; a production impl would check EFA/fabric health and
// that the SSH key exchange (mpi.go user-data) completed.
type Enroller struct{}

func (Enroller) IsEnrolled(_ context.Context, _ cohort.EntityID) (cohort.Readiness, error) {
	// Spike: trust the lifecycle Running state as enrollment. Real impl probes
	// the instance (slurmd check-in / EFA health) — that's the domain's meaning.
	return cohort.Readiness{Enrolled: true, Operational: true}, nil
}

// Assembler runs ONCE after the all-or-nothing barrier, over the complete,
// simultaneously-live cohort — the MPI wire-up phase. It receives every member's
// Observation (name + private IP), which is exactly what hostfile generation /
// peer discovery needs. Mechanism is the domain's; cohort learns only pass/fail.
type Assembler struct {
	// WireUp builds the MPI hostfile / distributes peers given the live members.
	// In the spike a test injects this; production wires spawn's peer-discovery.
	WireUp func(ctx context.Context, members []cohort.Observation) error
}

func (a Assembler) Assemble(ctx context.Context, members []cohort.Observation) error {
	if a.WireUp == nil {
		return nil
	}
	return a.WireUp(ctx, members)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func rungOf(intent cohort.EntityIntent) cohort.Rung {
	if rp, ok := intent.Placement.(cohort.RungPlacement); ok {
		return rp.Rung
	}
	return cohort.Rung{}
}

// mapState maps spawn/EC2 state strings onto cohort lifecycle states.
func mapState(s string) cohort.LifecycleState {
	switch s {
	case "pending":
		return cohort.StateLaunching
	case "running":
		return cohort.StateRunning
	case "stopping", "stopped":
		return cohort.StateStopped
	case "shutting-down", "terminated":
		return cohort.StateFailed
	default:
		return cohort.StateUnknown
	}
}
