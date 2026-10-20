package reaperdeploy

import (
	"context"
	"testing"
)

// #1157: teardown can only remove the bucket deploy used if deploy records it.
// Both deploy paths must tag the function with the bucket, and Inspect must
// read it back.
func TestDeployRecordsTheArtifactBucketOnTheFunction(t *testing.T) {
	opts := Options{AccountID: "123456789012", Region: "us-east-1", Version: "0.137.0", Artifact: "/tmp/x.zip", Bucket: "my-own-bucket"}

	t.Run("create", func(t *testing.T) {
		lamF := &fakeLambda{}
		d := newTestDeployer(&fakeIAM{}, lamF, &fakeEvents{}, &fakeS3{})
		if _, err := d.Deploy(context.Background(), opts); err != nil {
			t.Fatalf("Deploy: %v", err)
		}
		if got := lamF.createTags[ArtifactBucketTag]; got != "my-own-bucket" {
			t.Errorf("CreateFunction tagged %s=%q, want my-own-bucket", ArtifactBucketTag, got)
		}
	})

	t.Run("update", func(t *testing.T) {
		lamF := &fakeLambda{exists: true, dryRun: "true"}
		d := newTestDeployer(&fakeIAM{}, lamF, &fakeEvents{}, &fakeS3{})
		if _, err := d.Deploy(context.Background(), opts); err != nil {
			t.Fatalf("Deploy: %v", err)
		}
		if got := lamF.tags[ArtifactBucketTag]; got != "my-own-bucket" {
			t.Errorf("TagResource on redeploy set %s=%q, want my-own-bucket", ArtifactBucketTag, got)
		}
	})

	t.Run("default bucket is recorded too", func(t *testing.T) {
		lamF := &fakeLambda{}
		d := newTestDeployer(&fakeIAM{}, lamF, &fakeEvents{}, &fakeS3{})
		o := opts
		o.Bucket = ""
		if _, err := d.Deploy(context.Background(), o); err != nil {
			t.Fatalf("Deploy: %v", err)
		}
		if got, want := lamF.createTags[ArtifactBucketTag], DefaultBucketName(o.AccountID, o.Region); got != want {
			t.Errorf("recorded bucket %q, want the default %q", got, want)
		}
	})

	t.Run("inspect reads it back", func(t *testing.T) {
		lamF := &fakeLambda{exists: true, getTags: map[string]string{ArtifactBucketTag: "my-own-bucket"}}
		d := newTestDeployer(&fakeIAM{}, lamF, &fakeEvents{}, &fakeS3{})
		info, err := d.Inspect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.ArtifactBucket != "my-own-bucket" {
			t.Errorf("Inspect ArtifactBucket = %q", info.ArtifactBucket)
		}
	})
}
