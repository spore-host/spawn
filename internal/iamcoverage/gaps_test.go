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

	// The dashboard's member-account terminate and sweep-cancel paths have no
	// role that may do them. The read role is assumable by every signed-in
	// dashboard user directly (the browser does), so a terminate grant on it
	// would bypass the Lambda's ownership check; and SpawnSweepCrossAccountRole's
	// trust names only the sweep orchestrator. Owner decision (spawn#1349); the
	// dashboard answers a denied terminate with the CLI command. The
	// sts:AssumeRole entry also covers cross_account.go's assume, which runs on
	// the caller's credentials and needs no grant here (#815).
	"dashboard-api | SpawnDashboardLambdaRole | sts:AssumeRole":                           "#1349",
	"dashboard-api | SpawnDashboardCrossAccountReadRole | ec2:TerminateInstances":         "#1349",
	"dashboard-api | SpawnDashboardCrossAccountReadRole | ec2:CancelSpotInstanceRequests": "#1349",
}
