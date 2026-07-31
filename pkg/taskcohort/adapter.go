// Package taskcohort adapts spawn's AWS launch surface onto the cohort
// reconciliation core (github.com/spore-host/cohort) for a POOL of fungible
// worker instances that drain a shared task queue.
//
// It is the task-fan-out counterpart to pkg/mpicohort. Where mpicohort drives an
// MPI cluster — a COLLECTIVE cohort with an all-or-nothing barrier, per-index
// user-data, placement groups, and an SSM peers-file Assembler — a worker pool is
// the EASY case mpicohort's docs flagged:
//
//   - Workers are FUNGIBLE. There is no rank-0, no per-index user-data; every
//     worker runs the same pull-loop bootstrap. So there is no per-entity Configs
//     map — one BaseConfig fits all, overlaid only with the per-entity Name and
//     idempotency token.
//   - Placement is PER-ENTITY. A worker pool needs no placement group and no EFA
//     fabric, so a member that can't get capacity in one AZ can fall back
//     independently — no collective AZ invariant to preserve. The caller builds a
//     partial cohort (MinViable = floor), so the pool is best-effort: ask for N,
//     get M, and the tasks drain through whatever workers came up.
//   - There is NO Assembler and NO Enroller. Workers don't need to discover each
//     other or reach an "operational" barrier before work starts; each worker
//     begins pulling from the queue as soon as it boots. Readiness is "the queue
//     drains", observed out-of-band, not a cohort phase.
//
// This is why the pool path reuses cohort UNMODIFIED: NewPartialCohort +
// Actuator.Start (warm-pool resume) + RungPlacement's per-entity AZ fallback
// already express every semantic a best-effort/eventual worker pool needs.
package taskcohort

import (
	"context"
	"errors"
	"fmt"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
)

// LaunchAPI is the slice of *aws.Client the adapter needs. An interface (not the
// concrete *aws.Client) so tests can inject a fake without real AWS — the same
// seam mpicohort uses.
type LaunchAPI interface {
	Launch(ctx context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error)
	Terminate(ctx context.Context, region, instanceID string) error
	StartInstance(ctx context.Context, region, instanceID string) error
	ListInstances(ctx context.Context, region, stateFilter string) ([]aws.InstanceInfo, error)
}

// Actuator drives a single named worker instance via spawn's launcher. Per
// cohort's contract it NEVER operates on counts — one call names exactly one
// worker. Every worker shares BaseConfig (the pull-loop bootstrap user-data,
// instance type, IAM profile, tags); only the Name, idempotency token, and the
// rung's placement are overlaid per entity.
type Actuator struct {
	Client LaunchAPI
	Region string

	// BaseConfig is the pool-wide launch template: the queue-drainer user-data,
	// the sized instance type, the scoped IAM profile, the pool tag. Because
	// workers are fungible there is no per-entity config map (contrast
	// mpicohort.Actuator.Configs) — this one template launches every worker.
	BaseConfig aws.LaunchConfig
}

func (a *Actuator) Launch(ctx context.Context, intent cohort.EntityIntent) (cohort.Observation, error) {
	cfg := a.BaseConfig
	cfg.Region = a.Region
	cfg.Name = string(intent.ID)
	cfg.ClientToken = intent.IdempotencyToken // deterministic — safe to re-issue

	// Overlay the rung's per-entity placement. Unlike mpicohort there is no
	// collective placement group: a worker just takes an instance type + AZ from
	// its current rung, and can fall back independently of its siblings.
	if rp, ok := intent.Placement.(cohort.RungPlacement); ok {
		if rp.Rung.InstanceType != "" {
			cfg.InstanceType = rp.Rung.InstanceType
		}
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
		Address:    res.PrivateIP,
		Rung:       rungOf(intent),
	}, nil
}

// Start resumes a Stopped/Hibernated worker — the warm-pool path. cohort calls
// this instead of Launch when a member's placement is marked WarmStart, so a
// pool that was scaled to zero (stopped, not terminated) can be revived without
// re-bootstrapping.
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

// Stop is intentionally unsupported for a worker pool: workers drain to $0 by
// self-terminating on idle-timeout (the scale-to-zero property), not by a
// control-plane Stop. cohort only calls Stop on an explicit suspend path, which
// the pool does not use; a nil-safe error keeps the Actuator honest if it ever is.
func (a *Actuator) Stop(_ context.Context, _ cohort.EntityID, _ cohort.StopMode) error {
	return errors.New("taskcohort: Stop is not supported (workers self-terminate on idle-timeout)")
}

func (a *Actuator) Terminate(ctx context.Context, id cohort.EntityID) error {
	pid, err := a.providerID(ctx, id)
	if err != nil {
		// Already gone / never created is success for an idempotent Terminate.
		return nil
	}
	return a.Client.Terminate(ctx, a.Region, pid)
}

// providerID resolves an EntityID (the worker's Name tag) to an EC2 instance ID.
func (a *Actuator) providerID(ctx context.Context, id cohort.EntityID) (string, error) {
	return resolveProviderID(ctx, a.Client, a.Region, id)
}

func resolveProviderID(ctx context.Context, client LaunchAPI, region string, id cohort.EntityID) (string, error) {
	insts, err := client.ListInstances(ctx, region, "")
	if err != nil {
		return "", err
	}
	for _, in := range insts {
		if in.Name == string(id) {
			return in.InstanceID, nil
		}
	}
	return "", fmt.Errorf("taskcohort: no instance named %q", id)
}

// Observer reports infrastructure-truth state for named workers. It tolerates
// eventual consistency: a worker it can't find is StateUnknown, never
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

// Classifier maps a spawn launch error into exactly one cohort Fault class. It is
// identical in spirit to mpicohort's — capacity errors advance the fallback
// ladder, throttles back off, everything else is terminal — because fault
// classification is a property of the AWS error, not of what the instance will do.
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
		return cohort.Fault{Class: cohort.FaultCapacityExhausted, Code: code, Message: err.Error()}
	case "RequestLimitExceeded", "Throttling":
		return cohort.Fault{Class: cohort.FaultThrottle, Code: code, Message: err.Error()}
	default:
		return cohort.Fault{Class: cohort.FaultTerminal, Code: code, Message: err.Error()}
	}
}

// rungOf pulls the current Rung from a RungPlacement intent (zero Rung otherwise).
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
