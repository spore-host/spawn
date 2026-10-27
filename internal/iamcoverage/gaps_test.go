package iamcoverage

// knownGaps is every call TestEveryPrincipalCallIsGranted found ungranted that
// is not fixed yet. Each entry is "principal | role variant | action" and its
// reason, and each has an issue behind it. The gate fails on a gap that is not
// here, and on an entry here that is no longer a gap, so the ledger can only
// shrink by fixing something. The exceptions are calls the owner decided must
// stay denied; their reason says "(permanent)" and the comment above them why.
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

	// Permanent, by owner decision (spawn#1349), not open gaps: the dashboard
	// does not terminate an instance in a member account. Its only client there
	// is SpawnDashboardCrossAccountReadRole, which every signed-in dashboard
	// user can assume directly from the browser, so a terminate grant on it
	// would bypass the Lambda's ownership check and let any user terminate any
	// spore in the account. handleTerminateInstance's call is therefore always
	// denied, and the 403 names `spawn terminate <id>`, which runs as the user.
	// Sweep cancel needs neither: the dashboard marks the sweep cancelled and
	// the sweep orchestrator terminates its members.
	"dashboard-api | SpawnDashboardCrossAccountReadRole | ec2:TerminateInstances":         "#1349 (permanent)",
	"dashboard-api | SpawnDashboardCrossAccountReadRole | ec2:CancelSpotInstanceRequests": "#1349 (permanent)",
}
