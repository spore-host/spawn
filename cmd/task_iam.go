package cmd

import (
	"github.com/spore-host/spawn/pkg/taskiam"
)

// The scoped S3 staging policy and its helpers live in pkg/taskiam, shared
// with pkg/tasklaunch so `spawn task run`, spore-host-mcp and cmd's launch,
// sweep and pool paths grant from one implementation. These are the names
// cmd has always used for them.
var (
	taskStagingPolicy      = taskiam.StagingPolicy
	taskStagingPolicyFor   = taskiam.StagingPolicyFor
	taskExtraPolicies      = taskiam.ExtraPolicies
	ecrPullPolicies        = taskiam.ECRPullPolicies
	iamActionList          = taskiam.IAMActionList
	provenanceWriteActions = taskiam.ProvenanceWriteActions
	objectWriteActions     = taskiam.ObjectWriteActions
	s3Buckets              = taskiam.S3Buckets
	s3Buckets2             = taskiam.S3BucketsFromURIs
	s3Bucket               = taskiam.S3Bucket
)
