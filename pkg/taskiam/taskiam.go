// Package taskiam builds the scoped IAM grants a spawn task's instance profile
// carries: an S3 staging policy over exactly the buckets a TaskSpec reads and
// writes, and the policy templates a private-ECR image needs. Shared by
// pkg/tasklaunch (spawn task run, and spore-host-mcp through it) and by cmd's
// launch, sweep and pool paths, so they cannot drift apart.
package taskiam

import (
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/ecrref"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// S3Buckets extracts the distinct S3 bucket names from a manifest list's s3://
// endpoints (source or destination, whichever is the s3 side). Non-s3 entries
// (local paths) are skipped.
func S3Buckets(manifests []taskproto.Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range manifests {
		for _, ep := range []string{m.Source, m.Destination} {
			if b := S3Bucket(ep); b != "" && !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	return out
}

// S3BucketsFromURIs extracts distinct bucket names from a list of s3:// URIs (e.g.
// spec.Resources.S3ReadWrite).
func S3BucketsFromURIs(uris []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range uris {
		if b := S3Bucket(u); b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// S3Bucket returns the bucket name from an s3://bucket/key URI, or "" if uri is
// not an s3:// URI (e.g. a local path).
func S3Bucket(uri string) string {
	const p = "s3://"
	if !strings.HasPrefix(uri, p) {
		return ""
	}
	rest := uri[len(p):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// ExtraPolicies returns the IAM policy-template names (beyond the scoped S3
// staging policy) a task's instance profile needs. A private-ECR container image
// needs ecr:ReadOnly to pull; public images and non-container tasks need nothing
// extra. The names resolve to buildInlinePolicy templates (pkg/aws/iam.go);
// ecrref.Account is the shared private-ECR parser.
func ExtraPolicies(spec *taskproto.TaskSpec) []string {
	return ECRPullPolicies(spec.Container)
}

// ECRPullPolicies returns the ecr:ReadOnly policy template when any of images
// is a private-ECR ref, and nil otherwise. Shared by `spawn task run` (one
// image per task) and `spawn pool create` (the images its tasks run, #1148).
func ECRPullPolicies(images ...string) []string {
	for _, img := range images {
		if img != "" && ecrref.Account(img) != "" {
			return []string{"ecr:ReadOnly"}
		}
	}
	return nil
}

// ProvenanceWriteActions is what `spored provenance upload` needs on a write
// bucket beyond s3:PutObject (#1076): an upload that sets tags in the request
// needs s3:PutObjectTagging, and the provenance record is written with
// s3:PutObjectAnnotation. Both names are from AWS's machine-readable service
// reference. Used by `spawn task run` and pool workers, whose job scripts both
// upload with provenance.
var ProvenanceWriteActions = []string{"s3:PutObjectTagging", "s3:PutObjectAnnotation"}

// IAMActionList renders actions as the elements of a JSON array.
func IAMActionList(actions []string) string {
	q := make([]string, len(actions))
	for i, a := range actions {
		q[i] = fmt.Sprintf("%q", a)
	}
	return strings.Join(q, ",")
}

// StagingPolicy builds a scoped S3 policy granting read on the input buckets
// and write on the output + results buckets — the exact access the wrapper's
// aws s3 cp needs, and no more (preferred over the wildcard s3:FullAccess
// template). Modeled on GenerateScopedS3Policy.
func StagingPolicy(inputBuckets, outputBuckets []string, resultsBucket string, readWriteBuckets []string) string {
	return StagingPolicyFor(inputBuckets, outputBuckets, resultsBucket, readWriteBuckets, false)
}

// StagingPolicyFor is StagingPolicy, plus, when provenance is set, what
// `spored provenance upload` needs on the WRITE buckets (#1076): an upload that
// sets tags in the request requires s3:PutObjectTagging as well as PutObject,
// and the provenance record is written with s3:PutObjectAnnotation. Both action
// names are from AWS's machine-readable service reference. Only `spawn task
// run` asks for it; `spawn launch --s3-write` and sweeps never tag.
func StagingPolicyFor(inputBuckets, outputBuckets []string, resultsBucket string, readWriteBuckets []string, provenance bool) string {
	readB := DedupeBuckets(inputBuckets)
	writeB := DedupeBuckets(append(append([]string{}, outputBuckets...), resultsBucket))
	rwB := DedupeBuckets(readWriteBuckets)

	var stmts []string
	if len(readB) > 0 {
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectVersion"],"Resource":[%s]}`, bucketObjectARNs(readB)))
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%s]}`, bucketARNs(readB)))
	}
	// Only grant write when there is somewhere to write (#669). This statement
	// used to be unconditional, but on the launch path outputBuckets is empty and
	// resultsBucket is "" — and DedupeBuckets drops empty strings — so
	// `--s3-read bucket` alone shipped {"Action":["s3:PutObject"],"Resource":[]}.
	// IAM rejects the WHOLE document for that, so a read-only grant made the
	// instance profile impossible to create and failed the entire launch.
	if len(writeB) > 0 {
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:PutObject"],"Resource":[%s]}`, bucketObjectARNs(writeB)))
		if provenance {
			stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":[%s],"Resource":[%s]}`, IAMActionList(ProvenanceWriteActions), bucketObjectARNs(writeB)))
		}
	}
	// Full read-write buckets (e.g. Snakemake's S3 storage plugin, which lists,
	// reads, writes, and deletes across its storage bucket): grant object-level
	// Get/Put/Delete + bucket-level List/GetLocation on the whole bucket.
	if len(rwB) > 0 {
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectVersion","s3:PutObject","s3:DeleteObject"],"Resource":[%s]}`, bucketObjectARNs(rwB)))
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%s]}`, bucketARNs(rwB)))
	}
	// A document with no statements is MalformedPolicyDocument just as surely as
	// one with an empty Resource, so guarding the write statement alone would
	// have swapped one invalid policy for another. Return nothing instead: both
	// PutRolePolicy call sites in pkg/aws/iam.go are guarded on
	// InlinePolicyJSON != "", so an empty document is simply not attached.
	if len(stmts) == 0 {
		return ""
	}
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(stmts, ",") + `]}`
}

// TaskRunPolicyFor is the instance policy of a `spawn task run` task:
// StagingPolicyFor with provenance, plus s3:GetObject on the staged objects
// its launcher uploads to the results bucket (#1274): the command and flush
// hook, which the bootstrap fetches with the instance role instead of carrying
// them in the user data. See StagedFilesResources for exactly which keys.
func TaskRunPolicyFor(inputBuckets, outputBuckets []string, resultsBucket string, readWriteBuckets []string) string {
	pol := StagingPolicyFor(inputBuckets, outputBuckets, resultsBucket, readWriteBuckets, true)
	if resultsBucket == "" {
		return pol
	}
	stmt := fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject"],"Resource":[%s]}`, quotedList(StagedFilesResources(resultsBucket)))
	// resultsBucket is set, so StagingPolicyFor granted it PutObject and the
	// document is not empty: append to its statement list.
	return strings.TrimSuffix(pol, "]}") + "," + stmt + "]}"
}

// stagedFilesSHA256Len is the length of the hex sha256 in a staged object's
// name.
const stagedFilesSHA256Len = 64

// StagedFilesKey is the key of a task's staged command and flush hook (#1274):
// bootstrap-<sha256>.sh beside its completion record, under taskPrefix, the
// key part of the task's results prefix (empty for the bucket root).
func StagedFilesKey(taskPrefix, sha256Hex string) string {
	name := StagedFilesKeyPrefix("") + sha256Hex + ".sh"
	if taskPrefix == "" {
		return name
	}
	return taskPrefix + "/" + name
}

// StagedFilesKeyPrefix is what every staged object key under taskPrefix starts
// with, for listing a previous attempt's.
func StagedFilesKeyPrefix(taskPrefix string) string {
	if taskPrefix == "" {
		return "bootstrap-"
	}
	return taskPrefix + "/bootstrap-"
}

// StagedFilesResources are the object ARNs a task's role may GetObject for its
// staged files: a bootstrap-<64 characters>.sh object at the bucket root or
// under any prefix of resultsBucket.
//
// Not the one task's own prefix, although that is the only object the role
// needs. Roles are shared: CreateOrGetInstanceProfile names a role by the hash
// of its policy, so every task with the same buckets gets the same role, and a
// grant naming the task's prefix would give each task_id a role of its own
// (thousands for one workflow fan-out, against an account quota of 1,000 by
// default, and nothing deletes them). Within one role, IAM cannot tell its
// instances apart anyway. So the grant is the narrowest that does not vary by
// task: the staged objects' own name shape, in the results bucket only.
//
// Trust model. Every task role on a results bucket can GetObject every staged
// object in it, given its key. That is safe because such a role cannot list
// the bucket (StagingPolicyFor grants the results bucket only PutObject) and
// each key is the sha256 of bytes that include the attempt's random run id
// (tasklaunch.TestStagedKeyIsUnguessablePerRun), so no task can name another's
// object. A task whose role CAN list its results bucket, because the bucket is
// one of its inputs or in resources.s3_read_write, is never staged
// (tasklaunch.stagingRefusal). Such a role also gets GetObject on the whole
// bucket, so it can read every staged object there whether or not it is staged
// itself, as can any other principal with read on the bucket (a pool worker,
// whose spec bucket is the default results bucket, or `spawn launch --s3-read`
// on it): the guard keeps that task's own files out of the bucket, it does not
// hide other tasks' files from it.
func StagedFilesResources(resultsBucket string) []string {
	name := "bootstrap-" + strings.Repeat("?", stagedFilesSHA256Len) + ".sh"
	return []string{
		"arn:aws:s3:::" + resultsBucket + "/" + name,
		"arn:aws:s3:::" + resultsBucket + "/*/" + name,
	}
}

func quotedList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ",")
}

func DedupeBuckets(buckets []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range buckets {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

func bucketARNs(buckets []string) string {
	arns := make([]string, len(buckets))
	for i, b := range buckets {
		arns[i] = fmt.Sprintf("%q", "arn:aws:s3:::"+b)
	}
	return strings.Join(arns, ",")
}

func bucketObjectARNs(buckets []string) string {
	arns := make([]string, len(buckets))
	for i, b := range buckets {
		arns[i] = fmt.Sprintf("%q", "arn:aws:s3:::"+b+"/*")
	}
	return strings.Join(arns, ",")
}
