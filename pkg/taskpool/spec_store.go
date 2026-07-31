package taskpool

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3API is the slice of the S3 SDK the spec store needs — an interface so tests
// (and Substrate) inject a client, matching the SQSAPI / LaunchAPI seam pattern.
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// SpecStore stages TaskSpec JSON to S3 and fetches it back. A task's spec can
// carry a long staging script (nf-spawn embeds its whole staging script as the
// command), which can exceed SQS's 256 KiB message cap — so the submitter stages
// the spec to S3 and enqueues only a small TaskRef{task_id, spec_uri}; the worker
// fetches the spec by URI when it claims the ref. SpecStore implements SpecFetcher.
type SpecStore struct {
	Client S3API
	// Bucket + Prefix locate staged specs: s3://<bucket>/<prefix>/<run>/<task>.json.
	Bucket string
	Prefix string
}

// specKey is the object key for a run's task spec.
func (s *SpecStore) specKey(runID, taskID string) string {
	p := s.Prefix
	if p != "" && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return fmt.Sprintf("%s/pool-specs/%s/%s.json", p, runID, sanitize(taskID))
}

// Stage writes a task's spec JSON to S3 and returns its s3:// URI, to be carried
// on the queue ref. Idempotent: staging the same run/task overwrites, which is
// safe (deterministic content).
func (s *SpecStore) Stage(ctx context.Context, runID, taskID string, specJSON []byte) (string, error) {
	key := s.specKey(runID, taskID)
	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(specJSON),
	})
	if err != nil {
		return "", fmt.Errorf("stage spec for %s: %w", taskID, err)
	}
	return fmt.Sprintf("s3://%s/%s", s.Bucket, key), nil
}

// Fetch loads a staged spec by its s3:// URI (implements SpecFetcher — the worker
// side). It parses the bucket/key out of the URI the submitter stored on the ref.
func (s *SpecStore) Fetch(ctx context.Context, specURI string) ([]byte, error) {
	bucket, key, err := parseS3URI(specURI)
	if err != nil {
		return nil, err
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("fetch spec %s: %w", specURI, err)
	}
	defer func() { _ = out.Body.Close() }()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(out.Body); err != nil {
		return nil, fmt.Errorf("read spec %s: %w", specURI, err)
	}
	return buf.Bytes(), nil
}

// parseS3URI splits "s3://bucket/key/with/slashes" into bucket and key.
func parseS3URI(uri string) (bucket, key string, err error) {
	const p = "s3://"
	if len(uri) <= len(p) || uri[:len(p)] != p {
		return "", "", fmt.Errorf("not an s3:// URI: %q", uri)
	}
	rest := uri[len(p):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[:i], rest[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("s3 URI has no key: %q", uri)
}
