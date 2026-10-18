//go:build e2e_tier0

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Tier 0 coverage for `spawn results retention` (#1076, phase 2).
//
// Like footprint, the report's value is that it can't be misread and that it
// changes nothing: it feeds future tiering and deletion decisions. Substrate
// serves the listing and tag reads. The annotation read and input re-checks
// are unit-tested against a fake in pkg/provenance and were verified against
// real S3.

// An empty listing must say where outputs usually are, not read as "nothing to
// keep": task outputs go to each spec's own destination.
func TestTier0_ResultsRetention_EmptyBucketSaysWhereToLook(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.seedBucket("retention-empty")
	flat := flatten(env.runOK("results", "retention", "--bucket", "retention-empty"))
	for _, want := range []string{"nothing is tiered, tagged or deleted", "No objects under", "--bucket"} {
		if !strings.Contains(flat, want) {
			t.Errorf("empty report lacks %q:\n%s", want, flat)
		}
	}
}

// Objects without spawn:run-id are untracked and kept; a run-tagged output
// that didn't opt in is reported as not provable. Afterwards every object is
// still there with its tags unchanged: the report is read-only.
func TestTier0_ResultsRetention_ReportsAndChangesNothing(t *testing.T) {
	env := startSpawnSubstrate(t)
	const b = "retention-probe"
	env.seedBucket(b)
	ctx := context.Background()
	// Tags are seeded with PutObjectTagging: Substrate ignores the
	// x-amz-tagging header on PutObject (substrate#1434), which real S3 honours and which
	// spored's upload uses.
	put := func(key string, tags map[string]string) {
		if _, err := env.S3Client().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String(key), Body: strings.NewReader("x")}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		if len(tags) == 0 {
			return
		}
		var set []s3types.Tag
		for k, v := range tags {
			set = append(set, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		if _, err := env.S3Client().PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String(b), Key: aws.String(key),
			Tagging: &s3types.Tagging{TagSet: set}}); err != nil {
			t.Fatalf("tag %s: %v", key, err)
		}
	}
	put("tasks/t1/completion.json", nil)
	put("out/run-1/a.txt", map[string]string{"spawn:run-id": "run-1", "spawn:regen-class": "cheap", "spawn:retain": "false",
		"spawn:regen-usd": "0.0100", "spawn:regen-hours": "0.1000"})

	flat := flatten(env.runOK("results", "retention", "--bucket", b))
	for _, want := range []string{
		"nothing is tiered, tagged or deleted",
		"Runs (1)",
		"Untracked: 1 object(s)",
		"run-1 can't be proven regenerable",
		"not opted in",
		"Storage $ are static",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("report lacks %q:\n%s", want, flat)
		}
	}

	list, err := env.S3Client().ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Contents) != 2 {
		t.Errorf("after the report the bucket holds %d objects, want the 2 seeded", len(list.Contents))
	}
	tags, err := env.S3Client().GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("out/run-1/a.txt")})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags.TagSet) != 5 {
		t.Errorf("the report changed an output's tags: %v", tags.TagSet)
	}
}
