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

// Tier 0 coverage for `spawn results lifecycle` and `spawn results retain`
// (#1076, phase 3).

func (e *spawnEnv) lifecycleRuleIDs(bucket string) []string {
	e.t.Helper()
	out, err := e.S3Client().GetBucketLifecycleConfiguration(context.Background(), &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchLifecycleConfiguration") {
			return nil
		}
		e.t.Fatalf("read lifecycle: %v", err)
	}
	var ids []string
	for _, r := range out.Rules {
		ids = append(ids, aws.ToString(r.ID))
	}
	return ids
}

// The dry run changes nothing. --apply installs spawn's two rules and keeps the
// bucket's own rule, a second --apply is a no-op, and --remove takes spawn's
// rules out and leaves the user's.
func TestTier0_ResultsLifecycle_DryRunApplyRemove(t *testing.T) {
	env := startSpawnSubstrate(t)
	const b = "lifecycle-probe"
	env.seedBucket(b)
	ctx := context.Background()
	if _, err := env.S3Client().PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(b),
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: []s3types.LifecycleRule{{
			ID: aws.String("expire-scratch"), Status: s3types.ExpirationStatusEnabled,
			Filter:     &s3types.LifecycleRuleFilter{Prefix: aws.String("scratch/")},
			Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(7)},
		}}},
	}); err != nil {
		t.Fatalf("seed the user's rule: %v", err)
	}

	dry := flatten(env.runOK("results", "lifecycle", "--bucket", b))
	if !strings.Contains(dry, "dry run; nothing is changed") || !strings.Contains(dry, "expire-scratch") {
		t.Errorf("dry run output:\n%s", dry)
	}
	if ids := env.lifecycleRuleIDs(b); len(ids) != 1 {
		t.Fatalf("the dry run changed the bucket: rules %v", ids)
	}

	applied := flatten(env.runOK("results", "lifecycle", "--bucket", b, "--apply"))
	if !strings.Contains(applied, "read back to confirm") {
		t.Errorf("apply output:\n%s", applied)
	}
	got := strings.Join(env.lifecycleRuleIDs(b), ",")
	for _, want := range []string{"expire-scratch", "spawn-regen-cheap", "spawn-regen-moderate"} {
		if !strings.Contains(got, want) {
			t.Errorf("after --apply the bucket's rules are %s; missing %s", got, want)
		}
	}

	again := flatten(env.runOK("results", "lifecycle", "--bucket", b, "--apply"))
	if !strings.Contains(again, "already installed and current") {
		t.Errorf("a second --apply wasn't a no-op:\n%s", again)
	}

	env.runOK("results", "lifecycle", "--bucket", b, "--remove", "--apply")
	if ids := env.lifecycleRuleIDs(b); len(ids) != 1 || ids[0] != "expire-scratch" {
		t.Errorf("after --remove the bucket's rules are %v, want only the user's expire-scratch", ids)
	}
}

// retain sets spawn:retain on a spawn output and keeps every other tag, and
// leaves an object spawn didn't write alone.
func TestTier0_ResultsRetain_MergesTagsAndSkipsNonOutputs(t *testing.T) {
	env := startSpawnSubstrate(t)
	const b = "retain-probe"
	env.seedBucket(b)
	ctx := context.Background()
	put := func(key string, tags map[string]string) {
		if _, err := env.S3Client().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String(key), Body: strings.NewReader("x")}); err != nil {
			t.Fatal(err)
		}
		var set []s3types.Tag
		for k, v := range tags {
			set = append(set, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		if _, err := env.S3Client().PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String(b), Key: aws.String(key), Tagging: &s3types.Tagging{TagSet: set}}); err != nil {
			t.Fatal(err)
		}
	}
	put("out/a.txt", map[string]string{"spawn:run-id": "run-1", "spawn:regen-class": "cheap", "lab:project": "x"})
	put("out/notes.txt", map[string]string{"lab:project": "x"})

	out := flatten(env.runOK("results", "retain", "s3://"+b+"/out/"))
	if !strings.Contains(out, "set on 1") || !strings.Contains(out, "left alone 1") {
		t.Errorf("retain output:\n%s", out)
	}
	tags := func(key string) map[string]string {
		got, err := env.S3Client().GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, t := range got.TagSet {
			m[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
		return m
	}
	a := tags("out/a.txt")
	if a["spawn:retain"] != "true" || a["spawn:regen-class"] != "cheap" || a["lab:project"] != "x" || a["spawn:run-id"] != "run-1" {
		t.Errorf("the output's tags after retain: %v (spawn:retain=true added, every other tag kept)", a)
	}
	if n := tags("out/notes.txt"); len(n) != 1 || n["lab:project"] != "x" {
		t.Errorf("a non-spawn object's tags were changed: %v", n)
	}
}
