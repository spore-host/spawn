package s3transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// These tests assert the behaviour the deprecated manager package documented,
// which is what callers relied on before #932:
//
//   - "Uploads that are smaller than the Uploader's PartSize will be uploaded
//     using the PutObject API operation. Uploads that are larger than the
//     Uploader's PartSize will be uploaded using multipart upload."
//   - downloads are ranged GETs of PartSize bytes (DefaultDownloadPartSize, 5 MiB);
//   - with RequestChecksumCalculationWhenSupported (the default) a CRC32
//     checksum is sent, for a single PutObject and for a multipart upload.
//
// They run against the Substrate S3 emulator, never AWS.

const bucket = "s3transfer-test"

func setup(t *testing.T) (*s3.Client, *testutil.S3Recorder) {
	t.Helper()
	env := testutil.SubstrateServer(t)
	var rec *testutil.S3Recorder
	c := s3.NewFromConfig(env.AWSConfig, func(o *s3.Options) {
		rec = testutil.NewS3Recorder(o.HTTPClient)
		o.HTTPClient = rec
	})
	if _, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	rec.Reset()
	return c, rec
}

// payload returns n deterministic, non-repeating-per-part bytes.
func payload(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/4099)
	}
	return b
}

// upload writes body to a temp file and uploads it as callers do (an *os.File).
func upload(t *testing.T, c *tm.Client, key string, body []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "body")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := c.UploadObject(context.Background(), &tm.UploadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: f,
	}); err != nil {
		t.Fatalf("UploadObject %s (%d bytes): %v", key, len(body), err)
	}
}

func TestUploadIsSinglePutBelowOnePartAndMultipartAbove(t *testing.T) {
	cases := []struct {
		name      string
		size      int64
		opts      []func(*tm.Options)
		wantParts int // 0 = a single PutObject
	}{
		{"one byte under the default part", DefaultPartSize - 1, nil, 0},
		{"one byte over the default part", DefaultPartSize + 1, nil, 2},
		// transfermanager's own threshold is 16 MiB; manager's was the part size.
		{"between 5 MiB and transfermanager's 16 MiB threshold", 11 * 1024 * 1024, nil, 3},
		// WithPartSize must move the threshold with the part size.
		{"under a raised part size", 7 * 1024 * 1024, []func(*tm.Options){WithPartSize(8 * 1024 * 1024)}, 0},
		{"over a raised part size", 9 * 1024 * 1024, []func(*tm.Options){WithPartSize(8 * 1024 * 1024)}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s3c, rec := setup(t)
			body := payload(tc.size)
			upload(t, New(s3c, tc.opts...), "k", body)

			if tc.wantParts == 0 {
				if n := rec.Count(testutil.S3Request.PutObject); n != 1 {
					t.Errorf("PutObject calls = %d, want 1", n)
				}
				if n := rec.Count(testutil.S3Request.CreateMultipartUpload); n != 0 {
					t.Errorf("CreateMultipartUpload calls = %d, want 0", n)
				}
			} else {
				if n := rec.Count(testutil.S3Request.CreateMultipartUpload); n != 1 {
					t.Errorf("CreateMultipartUpload calls = %d, want 1", n)
				}
				if n := rec.Count(testutil.S3Request.UploadPart); n != tc.wantParts {
					t.Errorf("UploadPart calls = %d, want %d", n, tc.wantParts)
				}
			}

			got, err := s3c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("k")})
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			defer func() { _ = got.Body.Close() }()
			var buf bytes.Buffer
			if _, err := buf.ReadFrom(got.Body); err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(buf.Bytes()) != sha256.Sum256(body) {
				t.Errorf("stored object differs from what was uploaded (%d vs %d bytes)", buf.Len(), len(body))
			}
		})
	}
}

func TestUploadSendsACRC32Checksum(t *testing.T) {
	for _, size := range []int64{1024, DefaultPartSize + 1} {
		s3c, _ := setup(t)
		upload(t, New(s3c), "k", payload(size))
		head, err := s3c.HeadObject(context.Background(), &s3.HeadObjectInput{
			Bucket: aws.String(bucket), Key: aws.String("k"), ChecksumMode: s3types.ChecksumModeEnabled,
		})
		if err != nil {
			t.Fatalf("HeadObject: %v", err)
		}
		if aws.ToString(head.ChecksumCRC32) == "" {
			t.Errorf("%d-byte upload: no CRC32 checksum stored (type %q)", size, head.ChecksumType)
		}
	}
}

func TestDownloadIsRangedGetsOfOnePartEach(t *testing.T) {
	s3c, rec := setup(t)
	body := payload(12 * 1024 * 1024) // 3 ranges of 5 MiB: 5 + 5 + 2
	c := New(s3c)
	upload(t, c, "k", body)
	rec.Reset()

	p := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DownloadObject(context.Background(), &tm.DownloadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("k"), WriterAt: f,
	}); err != nil {
		t.Fatalf("DownloadObject: %v", err)
	}
	_ = f.Close()

	if n := rec.Count(testutil.S3Request.GetObject); n != 3 {
		t.Errorf("GET requests = %d, want 3 (12 MiB in 5 MiB ranges)", n)
	}
	want := "bytes=0-5242879"
	if n := rec.Count(func(q testutil.S3Request) bool { return q.GetObject() && q.Range == want }); n != 1 {
		t.Errorf("no GET with Range %q; downloads are not 5 MiB ranges", want)
	}
	if n := rec.Count(func(q testutil.S3Request) bool { return q.GetObject() && strings.Contains(q.RawQuery, "partNumber=") }); n != 0 {
		t.Errorf("%d part-number GETs; downloads must be ranged, not per uploaded part", n)
	}

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("downloaded %d bytes that differ from the %d uploaded", len(got), len(body))
	}
}

// A zero-byte object answers a ranged GET with 416; the old Downloader treated
// that as an empty object, and pipeline outputs can legitimately be empty.
func TestDownloadOfAnEmptyObjectSucceeds(t *testing.T) {
	s3c, _ := setup(t)
	if _, err := s3c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("empty"), Body: bytes.NewReader(nil),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	p := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := New(s3c).DownloadObject(context.Background(), &tm.DownloadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("empty"), WriterAt: f,
	}); err != nil {
		t.Fatalf("DownloadObject of an empty object: %v", err)
	}
	if st, _ := os.Stat(p); st.Size() != 0 {
		t.Errorf("size = %d, want 0", st.Size())
	}
}
