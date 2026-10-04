package taskproto

import (
	"fmt"
	"strings"
)

// DefaultResultsBucket is the per-account, per-region bucket spawn creates and
// writes task records into when a spec names no results_prefix.
func DefaultResultsBucket(accountID, region string) string {
	return fmt.Sprintf("spawn-results-%s-%s", accountID, region)
}

// defaultResultsSubPrefix is the key prefix under the default bucket. Kept as the
// default so existing records stay where every adapter already looks for them.
const defaultResultsSubPrefix = "tasks"

// EffectiveResultsPrefix returns the s3:// prefix for ONE task's records —
// completion.json, .exitcode and command.log all land directly under it.
//
// Before this (spawn#646) the location was derived from the account and region
// with no override, so a consumer wanting its own layout had to read TWO places:
// its own prefix for the results that outputs[].destination put there, and the
// fixed spawn-results path for the terminal signal. outputs[] was always fully
// caller-controlled; only the record was not.
//
// Defaults to s3://spawn-results-<account>-<region>/tasks/<task_id>, so a spec
// that says nothing behaves exactly as before.
func EffectiveResultsPrefix(spec *TaskSpec, accountID, region string) string {
	base := strings.TrimSuffix(strings.TrimSpace(spec.ResultsPrefix), "/")
	if base == "" {
		base = fmt.Sprintf("s3://%s/%s",
			DefaultResultsBucket(accountID, region), defaultResultsSubPrefix)
	}
	return base + "/" + spec.TaskID
}

// SplitS3URI splits "s3://bucket/key/parts" into its bucket and key. The key is
// empty for a bare bucket URI. Returns ok=false for anything that is not an
// s3:// URI with a bucket, so a caller never silently treats a typo as a bucket
// name.
func SplitS3URI(uri string) (bucket, key string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(uri), "s3://")
	if !found || rest == "" {
		return "", "", false
	}
	bucket, key, _ = strings.Cut(rest, "/")
	if bucket == "" {
		return "", "", false
	}
	return bucket, strings.Trim(key, "/"), true
}

// IsDefaultResultsBucket reports whether bucket is the one spawn manages for this
// account and region.
//
// It decides whether spawn may CREATE the bucket. spawn creates its own; it must
// not create a bucket a caller named, because a typo would silently leave a stray
// bucket behind and report success — the opposite of what someone pointing at
// their own storage expects (spawn#646).
func IsDefaultResultsBucket(bucket, accountID, region string) bool {
	return bucket == DefaultResultsBucket(accountID, region)
}
