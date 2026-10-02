package cmd

import (
	"strings"
	"testing"
)

// TestS3BucketFlagsProduceScopedPolicy is spawn#614's S3 half: --s3-read/--s3-write
// give launch the same DECLARATIVE least-privilege capability the task path has,
// built by the same function so the two cannot drift.
//
// The alternatives the reporter was left with were a 403, --iam-policy s3:ReadOnly
// (Resource "*" — every bucket in the account), or AmazonS3FullAccess.
func TestS3BucketFlagsProduceScopedPolicy(t *testing.T) {
	policy := taskStagingPolicy([]string{"my-inputs"}, []string{"my-results"}, "", nil)

	for _, want := range []string{
		`"arn:aws:s3:::my-inputs/*"`, // read: objects
		`"arn:aws:s3:::my-inputs"`,   // read: ListBucket needs the bucket itself
		`"arn:aws:s3:::my-results/*"`,
		`"s3:GetObject"`,
		`"s3:ListBucket"`,
		`"s3:PutObject"`,
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy missing %s:\n%s", want, policy)
		}
	}

	// Scoped means scoped: no wildcard resource, and nothing about buckets the user
	// never named.
	if strings.Contains(policy, `"Resource":["*"]`) || strings.Contains(policy, `"Resource":"*"`) {
		t.Errorf("policy must not grant on a wildcard resource:\n%s", policy)
	}
	if strings.Contains(policy, "some-other-bucket") {
		t.Errorf("policy mentions a bucket that was never named:\n%s", policy)
	}
	// A read bucket must not become writable.
	if strings.Contains(policy, `"s3:PutObject"],"Resource":["arn:aws:s3:::my-inputs/*"]`) {
		t.Errorf("a --s3-read bucket must not get write access:\n%s", policy)
	}
}

// TestValidateS3BucketFlagsRefusesWildcards is the security guard. The name is
// interpolated into a Resource ARN, so a wildcard would widen the grant to every
// matching bucket in the account — the exact over-broad outcome these flags exist to
// avoid.
func TestValidateS3BucketFlagsRefusesWildcards(t *testing.T) {
	for _, bad := range []string{"*", "my-*", "pre?fix"} {
		if err := validateS3BucketFlags([]string{bad}, nil); err == nil {
			t.Errorf("--s3-read %q must be refused", bad)
		}
		if err := validateS3BucketFlags(nil, []string{bad}); err == nil {
			t.Errorf("--s3-write %q must be refused", bad)
		}
	}
}

// TestValidateS3BucketFlagsRejectsURIsAndARNs: these produce a malformed ARN that
// grants nothing, so the failure would look like #614's original 403 rather than like
// the typo it is. The error names the bucket the user probably meant.
func TestValidateS3BucketFlagsRejectsURIsAndARNs(t *testing.T) {
	err := validateS3BucketFlags([]string{"s3://my-bucket/prefix"}, nil)
	if err == nil {
		t.Fatal("an s3:// URI must be refused")
	}
	if !strings.Contains(err.Error(), `"my-bucket"`) {
		t.Errorf("the error should suggest the bare bucket name, got: %v", err)
	}

	for _, bad := range []string{"arn:aws:s3:::my-bucket", "my-bucket/key", ""} {
		if err := validateS3BucketFlags([]string{bad}, nil); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestValidateS3BucketFlagsAcceptsRealNames(t *testing.T) {
	ok := []string{"my-bucket", "a1b", "my.bucket.with.dots", "cookbook-942542972736-us-west-2"}
	if err := validateS3BucketFlags(ok, ok); err != nil {
		t.Errorf("valid bucket names were refused: %v", err)
	}

	// Uppercase and too-short names are not valid S3 bucket names.
	for _, bad := range []string{"MyBucket", "ab"} {
		if err := validateS3BucketFlags([]string{bad}, nil); err == nil {
			t.Errorf("%q is not a valid bucket name and must be refused", bad)
		}
	}
}

// TestS3FlagsAreAdditiveWithIAMPolicy documents why this lands as InlinePolicyJSON:
// CreateOrGetInstanceProfile attaches it as its own "spawn-scoped-policy" document,
// separate from the --iam-policy shorthand, so passing both keeps both rather than
// one silently replacing the other.
func TestS3FlagsAreAdditiveWithIAMPolicy(t *testing.T) {
	// The scoped policy is self-contained and valid on its own; that is what makes
	// it safe to attach alongside another document.
	policy := taskStagingPolicy([]string{"b"}, nil, "", nil)
	if !strings.HasPrefix(policy, `{"Version":"2012-10-17"`) {
		t.Errorf("the scoped policy must be a complete policy document, got:\n%s", policy)
	}
}
