package iamcoverage

import (
	"strings"
)

// servicePrefix maps an SDK service package (the last element of
// github.com/aws/aws-sdk-go-v2/service/<pkg>) to its IAM service prefix.
//
// Deliberately a closed table: a package that is not here is reported as
// unmapped, and the gate fails, rather than guessing that the package name is
// the prefix. The guess is right for most services and wrong for exactly the
// ones that bite — cloudwatchlogs is logs, eventbridge is events, efs is
// elasticfilesystem, apigatewaymanagementapi is execute-api.
var servicePrefix = map[string]string{
	"account":                  "account",
	"apigatewaymanagementapi":  "execute-api",
	"cloudformation":           "cloudformation",
	"cloudwatch":               "cloudwatch",
	"cloudwatchlogs":           "logs",
	"costexplorer":             "ce",
	"dynamodb":                 "dynamodb",
	"ebs":                      "ebs",
	"ec2":                      "ec2",
	"ecr":                      "ecr",
	"efs":                      "elasticfilesystem",
	"eventbridge":              "events",
	"fsx":                      "fsx",
	"iam":                      "iam",
	"imagebuilder":             "imagebuilder",
	"kms":                      "kms",
	"lambda":                   "lambda",
	"organizations":            "organizations",
	"pricing":                  "pricing",
	"resourcegroupstaggingapi": "tag",
	"route53":                  "route53",
	"s3":                       "s3",
	"scheduler":                "scheduler",
	"secretsmanager":           "secretsmanager",
	"servicequotas":            "servicequotas",
	"sns":                      "sns",
	"sqs":                      "sqs",
	"ssm":                      "ssm",
	"sts":                      "sts",
	"xray":                     "xray",
}

// opActions are the operations whose IAM action is not "<prefix>:<Op>". An
// operation not listed maps to itself.
//
// Getting one wrong fails closed: an operation mapped to an action that does
// not exist can never be granted, so the coverage gate fails on it and someone
// looks. The cases listed are from the Service Authorization Reference's
// "actions" tables, where the operation and the action are named differently.
//
// An empty list means the operation needs no IAM permission at all.
var opActions = map[string][]string{
	// GetCallerIdentity is not authorized by IAM: "No permissions are required
	// to perform this operation" (STS API reference). Proven live too — the
	// reaper's production role grants no sts:GetCallerIdentity and calls it
	// every invocation (pkg/reaperiam, grantedElsewhere).
	"sts:GetCallerIdentity": {},

	// S3 names many actions after the resource rather than the operation.
	"s3:HeadBucket":                         {"s3:ListBucket"},
	"s3:HeadObject":                         {"s3:GetObject"},
	"s3:ListObjects":                        {"s3:ListBucket"},
	"s3:ListObjectsV2":                      {"s3:ListBucket"},
	"s3:ListObjectVersions":                 {"s3:ListBucketVersions"},
	"s3:ListMultipartUploads":               {"s3:ListBucketMultipartUploads"},
	"s3:ListParts":                          {"s3:ListMultipartUploadParts"},
	"s3:ListBuckets":                        {"s3:ListAllMyBuckets"},
	"s3:GetBucketLifecycleConfiguration":    {"s3:GetLifecycleConfiguration"},
	"s3:PutBucketLifecycleConfiguration":    {"s3:PutLifecycleConfiguration"},
	"s3:DeleteBucketLifecycle":              {"s3:PutLifecycleConfiguration"},
	"s3:DeleteBucketTagging":                {"s3:PutBucketTagging"},
	"s3:GetBucketEncryption":                {"s3:GetEncryptionConfiguration"},
	"s3:PutBucketEncryption":                {"s3:PutEncryptionConfiguration"},
	"s3:DeleteBucketEncryption":             {"s3:PutEncryptionConfiguration"},
	"s3:GetPublicAccessBlock":               {"s3:GetBucketPublicAccessBlock"},
	"s3:PutPublicAccessBlock":               {"s3:PutBucketPublicAccessBlock"},
	"s3:DeletePublicAccessBlock":            {"s3:PutBucketPublicAccessBlock"},
	"s3:DeleteBucketOwnershipControls":      {"s3:PutBucketOwnershipControls"},
	"s3:CreateMultipartUpload":              {"s3:PutObject"},
	"s3:UploadPart":                         {"s3:PutObject"},
	"s3:CompleteMultipartUpload":            {"s3:PutObject"},
	"s3:UploadPartCopy":                     {"s3:PutObject", "s3:GetObject"},
	"s3:CopyObject":                         {"s3:PutObject", "s3:GetObject"},
	"s3:DeleteObjects":                      {"s3:DeleteObject"},
	"s3:GetObjectAttributes":                {"s3:GetObject", "s3:GetObjectAttributes"},
	"s3:GetBucketNotificationConfiguration": {"s3:GetBucketNotification"},
	"s3:PutBucketNotificationConfiguration": {"s3:PutBucketNotification"},

	// Lambda's Invoke family is all lambda:InvokeFunction.
	"lambda:Invoke":                   {"lambda:InvokeFunction"},
	"lambda:InvokeAsync":              {"lambda:InvokeFunction"},
	"lambda:InvokeWithResponseStream": {"lambda:InvokeFunction"},

	// The API Gateway management API is authorized as execute-api:ManageConnections.
	"execute-api:PostToConnection": {"execute-api:ManageConnections"},
	"execute-api:GetConnection":    {"execute-api:ManageConnections"},
	"execute-api:DeleteConnection": {"execute-api:ManageConnections"},
}

// featureOps maps the SDK's higher-level libraries that make operations on the
// caller's behalf, at their own API boundary. Their internals are not scanned
// (see the package doc), so every method spawn can call on them must be here or
// it is reported unmapped.
//
// The transfer manager splits a large upload into multipart parts and aborts
// the upload when a part fails. AbortMultipartUpload is its own action, so a
// role holding only s3:PutObject can upload but cannot clean up a failed
// multipart upload, and the orphaned parts bill until a lifecycle rule removes
// them.
var featureOps = map[string]map[string][]string{
	sdkRoot + "feature/s3/transfermanager": {
		"UploadObject":      {"s3:PutObject", "s3:AbortMultipartUpload"},
		"UploadDirectory":   {"s3:PutObject", "s3:AbortMultipartUpload"},
		"DownloadObject":    {"s3:GetObject"},
		"GetObject":         {"s3:GetObject"},
		"DownloadDirectory": {"s3:ListBucket", "s3:GetObject"},
	},
}

// noIAMPackages are SDK feature packages whose operations are not AWS API
// calls and need no IAM permission. IMDS is the instance metadata service, a
// link-local endpoint on the instance itself.
var noIAMPackages = map[string]bool{
	sdkRoot + "feature/ec2/imds": true,
}

// credentialProviders are SDK credential providers spawn constructs itself
// whose use is an operation authorized by the CALLER's policy. Assuming a role
// needs sts:AssumeRole in the caller's identity policy as well as the target's
// trust policy.
var credentialProviders = map[string][]string{
	sdkRoot + "credentials/stscreds.NewAssumeRoleProvider": {"sts:AssumeRole"},
}

// signedServices are the SigV4 signing names spawn signs requests for by hand,
// and what such a request is authorized as. spored and onboarding call a Lambda
// Function URL with AWS_IAM auth this way (#173): the signing name is "lambda"
// and the action is lambda:InvokeFunctionUrl, not lambda:InvokeFunction.
var signedServices = map[string][]string{
	"lambda": {"lambda:InvokeFunctionUrl"},
}

// mapOp translates one recognised operation.
func mapOp(pkg, op, display string) (foundOp, bool) {
	if noIAMPackages[pkg] {
		return foundOp{display: display}, true
	}
	if ops, ok := featureOps[pkg]; ok {
		acts, ok := ops[op]
		if !ok {
			return foundOp{display: display, err: "no IAM actions mapped for " + pkgBase(pkg) + "." + op + " (add it to featureOps)"}, true
		}
		return foundOp{display: display, actions: acts, key: pkg + "." + op}, true
	}
	svcPkg, ok := strings.CutPrefix(pkg, sdkServicePrefix)
	if !ok {
		// An *Input type from elsewhere in the SDK (a feature package not in
		// featureOps). Report it: it may be making calls nobody has mapped.
		return foundOp{display: display, err: "SDK package " + pkg + " is neither a service nor a mapped feature package"}, true
	}
	if i := strings.Index(svcPkg, "/"); i >= 0 {
		// service/<svc>/types etc. hold no operations.
		return foundOp{}, false
	}
	prefix, ok := servicePrefix[svcPkg]
	if !ok {
		return foundOp{display: display, err: "no IAM service prefix for SDK package service/" + svcPkg + " (add it to servicePrefix)"}, true
	}
	return foundOp{display: display, actions: ActionsFor(prefix, op), key: prefix + ":" + op}, true
}

// ActionsFor is the IAM actions an operation of an IAM service needs.
func ActionsFor(prefix, op string) []string {
	key := prefix + ":" + op
	if acts, ok := opActions[key]; ok {
		return acts
	}
	return []string{key}
}
