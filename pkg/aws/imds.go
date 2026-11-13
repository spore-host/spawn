package aws

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Instance metadata options on every launch (#1588).
//
// Every RunInstances spawn makes requires IMDSv2 session tokens
// (HttpTokens=required), with the endpoint enabled and a PUT response hop limit
// of IMDSHopLimit. Before #1588 only a compliance-mode launch set them, so a
// launch from a custom --ami, an app-images or `spawn create-ami` image,
// Ubuntu or an older Windows AMI, in an account with no IMDS default, accepted
// IMDSv1: an SSRF in a web app or a workload then read the instance role's
// credentials with a single GET.
//
// The hop limit. 1 keeps the token on the instance: a process in a container on
// a bridge network is one hop further and gets no token, so it cannot reach the
// instance role. That is the point for a web app (`spawn app launch` web kinds
// publish the container's port on the internet, and the app role reads the
// wildcard TLS key) and is harmless where the container shares the host's
// network (DCV container apps run --network host). Where a bridged container
// is the workload and is expected to use the instance role, the launch asks for
// IMDSHopLimitBridgedContainers instead:
//
//   - `spawn task run` with a container (pkg/tasklaunch): adapters whose own
//     tooling does the S3 I/O run inside the task container
//     (resources.s3_read_write, e.g. Snakemake's S3 storage plugin).
//   - pkg/launcher Provision with a ContainerScript (pkg/userdata container
//     launches, used by lagotto and spore-host-mcp): the container is the
//     workload and the profile is its only credential.
//
// `spawn launch --imds-hop-limit` sets it for anything else.

// IMDSHopLimit is the PUT response hop limit a launch gets by default.
const IMDSHopLimit = 1

// IMDSHopLimitBridgedContainers is the hop limit of a launch whose workload is a
// container on Docker's bridge network that must reach the instance role.
const IMDSHopLimitBridgedContainers = 2

// MaxIMDSHopLimit is the largest hop limit EC2 accepts.
const MaxIMDSHopLimit = 64

// IMDSMetadataOptions is the MetadataOptions every spawn RunInstances carries:
// IMDSv2 required, the endpoint enabled, and hopLimit, or IMDSHopLimit when
// hopLimit is not positive.
func IMDSMetadataOptions(hopLimit int) *types.InstanceMetadataOptionsRequest {
	if hopLimit <= 0 {
		hopLimit = IMDSHopLimit
	}
	if hopLimit > MaxIMDSHopLimit {
		hopLimit = MaxIMDSHopLimit
	}
	return &types.InstanceMetadataOptionsRequest{
		HttpTokens:              types.HttpTokensStateRequired,
		HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		HttpPutResponseHopLimit: aws.Int32(int32(hopLimit)),
	}
}
