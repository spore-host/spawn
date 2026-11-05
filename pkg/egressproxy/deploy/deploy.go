// Package deploy finds or creates the spawn egress proxy of a VPC (#1515
// phase 3): the t4g.micro running `spored egress-proxy` that lets a
// --no-public-ip launch reach the few hosts outside AWS its bootstrap fetches
// from, when the VPC has no NAT. One proxy per VPC, shared by every launch in
// it, found by its tags and by what the instance itself cannot change.
//
// What lives here, each usable on its own by `spawn egress-proxy
// status|delete|upgrade` and the reaper (phase 5):
//
//   - [EnsureGroups] and [DeleteGroups]: the two security groups, and their
//     deleter.
//   - [Find] and [Evaluate]: which running proxy, if any, a launch may reuse.
//   - [Create], [CheckReady], [WaitReady] and [Terminate]: a new proxy, one
//     look at its readiness and the wait over those looks, and its teardown.
//   - [FindOrCreate]: the launch path's use of all of them, blocking.
//   - [EnsureSweep]: the sweep orchestrator's, one bounded look per poll, for
//     a detached sweep's recorded [SweepTarget] (#1515 PR B).
//
// Every AWS call goes through [EC2] and [Prober], so tests need no AWS.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
)

// Tags and names.
const (
	// ProxyTag marks an egress proxy instance, valued with its VPC ID. It is a
	// lookup key, not proof: see [Evaluate] for what is.
	ProxyTag = "spawn:egress-proxy"
	// InstanceType is the proxy's size. It carries a handful of bootstrap
	// downloads per launch, not a workload, but it must stay up for days: on a
	// t4g.nano (0.5 GiB) the bootstrap's dnf install of acpid drove the kernel
	// out of memory, and the OOM killer took spored and then systemd-resolved,
	// the proxy's DNS (live, #1515). 1 GiB leaves dnf room.
	InstanceType = "t4g.micro"
	// AMIParameter is the public SSM parameter the proxy's image comes from:
	// the latest AL2023 arm64, the parameter spawn's auto-AMI already reads
	// (pkg/aws AMI_AL2023_ARM). Each rotation boots it fresh, so the proxy is
	// never patched in place.
	AMIParameter = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
	// PurposeClients and PurposeProxy are the groups' spawn:purpose tags.
	PurposeClients = "egress-clients"
	PurposeProxy   = "egress-proxy"
)

// ClientsGroupName is the security group every opted-in member carries in
// addition to its own, and the only source the proxy admits.
func ClientsGroupName(vpcID string) string { return "spawn-egress-clients-" + vpcID }

// ProxyGroupName is the proxy instance's only security group.
func ProxyGroupName(vpcID string) string { return "spawn-egress-proxy-" + vpcID }

// InstanceName is the proxy's Name tag.
func InstanceName(vpcID string) string { return "spawn-egress-proxy-" + vpcID }

// EC2 is the slice of EC2 this package calls. *ec2.Client satisfies it.
type EC2 interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	CreateSecurityGroup(ctx context.Context, in *ec2.CreateSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(ctx context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	AuthorizeSecurityGroupEgress(ctx context.Context, in *ec2.AuthorizeSecurityGroupEgressInput, optFns ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupEgressOutput, error)
	RevokeSecurityGroupIngress(ctx context.Context, in *ec2.RevokeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupEgress(ctx context.Context, in *ec2.RevokeSecurityGroupEgressInput, optFns ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error)
	DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// Prober runs a shell command on an instance over SSM and waits for it.
// *pkg/aws.Client satisfies it.
type Prober interface {
	RunShellScript(ctx context.Context, region, instanceID, command string, timeout time.Duration) (*spawnaws.SSMRunResult, error)
}

// Proxy is an egress proxy instance.
type Proxy struct {
	InstanceID, PrivateIP, SubnetID, AvailabilityZone, Version, State string
	LaunchTime, Deadline                                              time.Time
	// Created reports that this call launched it.
	Created bool
}

// Addr is where members reach the proxy: its private IP and [egressproxy.Port].
func (p Proxy) Addr() string {
	return net.JoinHostPort(p.PrivateIP, strconv.Itoa(egressproxy.Port))
}

// errorsJoin keeps a teardown's error beside the failure that caused it.
var errorsJoin = errors.Join

func tagMap(tags []types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func fprintf(w io.Writer, format string, args ...any) {
	if w != nil {
		_, _ = fmt.Fprintf(w, format, args...)
	}
}

// sortedKeys is m's keys in order, for stable messages.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func join(xs []string) string { return strings.Join(xs, ", ") }
