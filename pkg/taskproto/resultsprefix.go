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

// StagingBucketPrefix starts the name of every spawn staging bucket.
const StagingBucketPrefix = "spawn-staging-"

// StagingBucket is the per-account, per-region bucket `spawn task run` stages
// a task's wrapper and flush hook in (#1274). It holds nothing else: spawn
// creates it private, unlistable by any role spawn makes, with every object
// expiring after a day, and a task's role can read only its own staged object
// by its unguessable key. No spec may name it as an input, output, results
// prefix or read-write bucket (see Validate).
func StagingBucket(accountID, region string) string {
	return StagingBucketPrefix + accountID + "-" + region
}

// IsStagingBucket reports whether bucket is a spawn staging bucket, of any
// account or region.
func IsStagingBucket(bucket string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(bucket)), StagingBucketPrefix)
}

// StagingBucketUses lists the fields of spec that name a spawn staging bucket,
// which Validate refuses: a grant on one would let the task's role read or
// list other tasks' staged files (#1274).
func StagingBucketUses(spec *TaskSpec) []string {
	var uses []string
	check := func(field, uri string) {
		if b, _, ok := SplitS3URI(strings.TrimSpace(uri)); ok && IsStagingBucket(b) {
			uses = append(uses, fmt.Sprintf("%s %q", field, uri))
		}
	}
	for i, m := range spec.Inputs {
		check(fmt.Sprintf("inputs[%d].source", i), m.Source)
		check(fmt.Sprintf("inputs[%d].destination", i), m.Destination)
	}
	for i, m := range spec.Outputs {
		check(fmt.Sprintf("outputs[%d].source", i), m.Source)
		check(fmt.Sprintf("outputs[%d].destination", i), m.Destination)
	}
	for i, u := range spec.Resources.S3ReadWrite {
		check(fmt.Sprintf("resources.s3_read_write[%d]", i), u)
	}
	check("results_prefix", spec.ResultsPrefix)
	return uses
}
