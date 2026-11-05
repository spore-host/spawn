// Package sweepproxy is the record of a detached parameter sweep's spawn
// egress proxies (#1515 PR B), as the sweep's DynamoDB record carries them:
// one [Target] per (region, VPC) whose members count on the proxy. The CLI
// writes it at submit time; the sweep orchestrator updates it as it ensures
// each proxy before a launch (pkg/egressproxy/deploy EnsureSweep).
//
// It is its own package, on the standard library alone, because pkg/sweep's
// record carries it and every Lambda that reads that record (the scheduler
// handler, the dashboard) would otherwise link the proxy's launch code.
package sweepproxy

import "time"

// Target phases.
const (
	// PhaseReady: ProxyID was ready at its last check. Before each launch it
	// is found again and re-evaluated; it is not re-probed.
	PhaseReady = "ready"
	// PhaseCreating: ProxyID was launched by the orchestrator at
	// CreatingSince and is not yet ready. The target's members are held.
	PhaseCreating = "creating"
	// PhaseFailed: no proxy could be made ready (FailReason). The target's
	// members are failed with that reason, not launched.
	PhaseFailed = "failed"
)

// Subnet is a public subnet of the VPC, routed to an internet gateway, where
// a proxy can run.
type Subnet struct {
	SubnetID         string `json:"subnet_id" dynamodbav:"subnet_id"`
	AvailabilityZone string `json:"availability_zone" dynamodbav:"availability_zone"`
}

// Target is one (region, VPC) of a detached sweep whose members count on the
// egress proxy.
type Target struct {
	Region string `json:"region" dynamodbav:"region"`
	VPCID  string `json:"vpc_id" dynamodbav:"vpc_id"`

	// What a new proxy is launched with: the CLI's inputs at submit time, so
	// a proxy the orchestrator launches is the one the CLI would have.
	//
	// AvailabilityZone is the first member's, which the subnet choice
	// prefers.
	AvailabilityZone string   `json:"availability_zone,omitempty" dynamodbav:"availability_zone,omitempty"`
	PublicSubnets    []Subnet `json:"public_subnets" dynamodbav:"public_subnets"`
	// Idle is the proxy's idle period (--egress-proxy-idle).
	Idle time.Duration `json:"idle" dynamodbav:"idle"`
	// Version is the submitting CLI's spawn version: the version its proxy
	// carries, so the one a reusable proxy must carry.
	Version string `json:"version" dynamodbav:"version"`
	// InstanceProfile is the spored instance profile, which the CLI ensured.
	InstanceProfile string `json:"instance_profile" dynamodbav:"instance_profile"`
	// AMI is the AL2023 arm64 image the CLI resolved.
	AMI string `json:"ami" dynamodbav:"ami"`
	// UserData is the proxy's encoded user data, rendered by the CLI, so it
	// installs the CLI's spored release.
	UserData string `json:"user_data" dynamodbav:"user_data"`
	// BaseTags are the proxy's standard launch tags but its launch time and
	// lifetime, which its launch sets.
	BaseTags map[string]string `json:"base_tags" dynamodbav:"base_tags"`

	// RenderedAddr and RenderedClientsGroupID are the proxy address and
	// clients group the uploaded members' user data and groups name.
	RenderedAddr           string `json:"rendered_addr" dynamodbav:"rendered_addr"`
	RenderedClientsGroupID string `json:"rendered_clients_group_id" dynamodbav:"rendered_clients_group_id"`

	// The proxy now.
	Phase          string `json:"phase" dynamodbav:"phase"`
	ProxyID        string `json:"proxy_id,omitempty" dynamodbav:"proxy_id,omitempty"`
	ProxyAddr      string `json:"proxy_addr,omitempty" dynamodbav:"proxy_addr,omitempty"`
	ClientsGroupID string `json:"clients_group_id,omitempty" dynamodbav:"clients_group_id,omitempty"`
	ProxyGroupID   string `json:"proxy_group_id,omitempty" dynamodbav:"proxy_group_id,omitempty"`
	// CreatingSince is when the orchestrator launched ProxyID
	// (PhaseCreating).
	CreatingSince time.Time `json:"creating_since,omitzero" dynamodbav:"creating_since,omitempty"`
	// Created are the proxies the orchestrator launched for this target. A
	// sweep cancel may terminate one of them that is still creating, and
	// nothing else.
	Created []string `json:"created,omitempty" dynamodbav:"created,omitempty"`
	// ErrorSince and LastError are a run of AWS errors before a proxy was
	// launched; the target fails when it outlasts the readiness budget.
	ErrorSince time.Time `json:"error_since,omitzero" dynamodbav:"error_since,omitempty"`
	LastError  string    `json:"last_error,omitempty" dynamodbav:"last_error,omitempty"`
	// FailReason is why the target is PhaseFailed: what its members are
	// failed with.
	FailReason string `json:"fail_reason,omitempty" dynamodbav:"fail_reason,omitempty"`
}

// Binding is what a member names of its VPC's proxy.
type Binding struct {
	// Addr is the proxy's host:port, in the bootstrap's http_proxy.
	Addr string
	// ClientsGroupID is the clients group, among the member's groups.
	ClientsGroupID string
}

// Rendered is what the uploaded members name.
func (t Target) Rendered() Binding {
	return Binding{Addr: t.RenderedAddr, ClientsGroupID: t.RenderedClientsGroupID}
}

// Current is what a member launched now must name. Valid in PhaseReady.
func (t Target) Current() Binding {
	return Binding{Addr: t.ProxyAddr, ClientsGroupID: t.ClientsGroupID}
}

// CreatingProxies are the proxies t's sweep orchestrator launched that are
// not yet ready: the only proxies a sweep cancel may terminate. A ready
// proxy, or one the CLI made, may be serving other sweeps and launches.
func (t Target) CreatingProxies() []string {
	if t.Phase != PhaseCreating || t.ProxyID == "" {
		return nil
	}
	for _, id := range t.Created {
		if id == t.ProxyID {
			return []string{id}
		}
	}
	return nil
}
