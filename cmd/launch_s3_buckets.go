package cmd

import (
	"fmt"
	"regexp"
	"strings"
)

// s3BucketNamePattern is AWS's bucket-naming grammar, narrowed: 3-63 chars of
// lowercase letters, digits, hyphens and dots, starting and ending alphanumeric.
var s3BucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// validateS3BucketFlags rejects anything that is not a plain bucket name before it
// reaches a policy document (#614).
//
// This matters more than ordinary input validation because the value is
// interpolated straight into a Resource ARN. A wildcard would silently widen the
// grant to every bucket in the account — exactly the over-broad outcome these flags
// exist to avoid — and an "s3://bucket/key" or full ARN would produce a malformed
// ARN that grants nothing, failing at runtime with a 403 that looks like the bug
// being fixed rather than like a typo.
func validateS3BucketFlags(read, write []string) error {
	for _, group := range []struct {
		flag    string
		buckets []string
	}{
		{"--s3-read", read},
		{"--s3-write", write},
	} {
		for _, b := range group.buckets {
			if err := validateS3BucketName(group.flag, b); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateS3BucketName(flag, name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return fmt.Errorf("%s: empty bucket name", flag)
	case strings.ContainsAny(trimmed, "*?"):
		return fmt.Errorf("%s %q: wildcards are not allowed — name each bucket exactly "+
			"(a wildcard here would grant access to every matching bucket in the account; "+
			"use --iam-policy s3:ReadOnly if you genuinely want account-wide access)", flag, name)
	case strings.HasPrefix(trimmed, "s3://"):
		return fmt.Errorf("%s %q: pass just the bucket name, not an s3:// URI (e.g. %q)",
			flag, name, strings.SplitN(strings.TrimPrefix(trimmed, "s3://"), "/", 2)[0])
	case strings.HasPrefix(trimmed, "arn:"):
		return fmt.Errorf("%s %q: pass just the bucket name, not an ARN", flag, name)
	case strings.Contains(trimmed, "/"):
		return fmt.Errorf("%s %q: pass just the bucket name, not a bucket/key path", flag, name)
	case !s3BucketNamePattern.MatchString(trimmed):
		return fmt.Errorf("%s %q: not a valid S3 bucket name (3-63 chars, lowercase letters, "+
			"digits, dots and hyphens, starting and ending alphanumeric)", flag, name)
	}
	return nil
}
