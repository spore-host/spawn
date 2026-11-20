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

	// The reaper's account-idle sweep (#1654) inventories calque through
	// calquefootprint.Inventory, whose Collect lists S3 objects only in the
	// buckets Options.Buckets names. The reaper names none (calque records
	// no buckets), so the call is unreachable at runtime, but the call graph
	// cannot see Options. It stays denied: the member role reads no S3
	// object or object tag (permanent).
	"ttl-reaper | cross-account TTLReaperCrossAccountRole | s3:GetObjectTagging": "#1654 (permanent)",
}
