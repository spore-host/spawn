package iamcoverage

// knownGaps is every call TestEveryPrincipalCallIsGranted found ungranted that
// is not fixed yet. Each entry is "principal | role variant | action" and its
// reason, and each has an issue behind it. The gate fails on a gap that is not
// here, and on an entry here that is no longer a gap, so the ledger can only
// shrink by fixing something.
//
// Do not add an entry to make the gate pass. A gap is a feature that fails at
// runtime with AccessDenied, usually silently. Grant the action in the policy
// spore.host provisions, or make the feature degrade with a message that names
// the permission, and only then decide whether an entry belongs here.
var knownGaps = map[string]string{
	// Opt-in tracing (off by default, not settable by `spawn launch`): an
	// enabled exporter would be denied (#1344, item 1).
	"spored | spawn-instance-* (spored-baseline-policy) | xray:PutTraceSegments": "#1344",
	"spored | spored-instance-role | xray:PutTraceSegments":                      "#1344",

	// A batch queue launched without --iam-role uploads its results under
	// the shared spored-instance-role, which has no write on any user bucket
	// (#1344, item 2).
	"spored run-queue | spored-instance-role | s3:PutObject":            "#1344",
	"spored run-queue | spored-instance-role | s3:AbortMultipartUpload": "#1344",

	// The dashboard role's setup script is far behind the code; the live role
	// is hand-managed (#1344, item 3).
	"dashboard-api | SpawnDashboardLambdaRole | dynamodb:PutItem":                              "#1344",
	"dashboard-api | SpawnDashboardLambdaRole | dynamodb:DeleteItem":                           "#1344",
	"dashboard-api | SpawnDashboardLambdaRole | secretsmanager:GetSecretValue":                 "#1344",
	"dashboard-api | SpawnDashboardLambdaRole | s3:GetObject":                                  "#1344",
	"dashboard-api | SpawnDashboardLambdaRole | s3:PutObject":                                  "#1344",
	"dashboard-api | SpawnDashboardLambdaRole | sts:AssumeRole":                                "#1344",
	"dashboard-websocket | SpawnDashboardLambdaRole | dynamodb:PutItem":                        "#1344",
	"dashboard-websocket | SpawnDashboardLambdaRole | dynamodb:DeleteItem":                     "#1344",
	"dashboard-websocket-processor | SpawnDashboardLambdaRole | dynamodb:DeleteItem":           "#1344",
	"dashboard-websocket-processor | SpawnDashboardLambdaRole | execute-api:ManageConnections": "#1344",
	"cost-history-collector | SpawnDashboardLambdaRole | dynamodb:PutItem":                     "#1344",

	// The member-account read role is AWS-managed policies only (wildcards,
	// not modelled), and the dashboard's terminate path assumes it to
	// terminate (#1344, item 4).
	"dashboard-api | SpawnDashboardCrossAccountReadRole | ec2:DescribeInstances":          "#1344 (AmazonEC2ReadOnlyAccess wildcard)",
	"dashboard-api | SpawnDashboardCrossAccountReadRole | ec2:TerminateInstances":         "#1344",
	"dashboard-api | SpawnDashboardCrossAccountReadRole | ec2:CancelSpotInstanceRequests": "#1344",
}
