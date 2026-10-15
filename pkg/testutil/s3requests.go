package testutil

import (
	"net/http"
	"strings"
	"sync"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
)

// S3Request is one HTTP request an SDK client sent, reduced to what tells S3
// operations apart.
type S3Request struct {
	Method   string
	RawQuery string
	Range    string
	// Header is a copy of the request's headers, for what the emulator does
	// not store (Substrate drops x-amz-tagging on PutObject).
	Header http.Header
}

// CreateMultipartUpload reports whether r is a CreateMultipartUpload call.
func (r S3Request) CreateMultipartUpload() bool {
	return r.Method == http.MethodPost && strings.Contains(r.RawQuery, "uploads")
}

// UploadPart reports whether r is an UploadPart call.
func (r S3Request) UploadPart() bool {
	return r.Method == http.MethodPut && strings.Contains(r.RawQuery, "partNumber=")
}

// PutObject reports whether r is a single PutObject call.
func (r S3Request) PutObject() bool {
	return r.Method == http.MethodPut && !strings.Contains(r.RawQuery, "partNumber=") &&
		!strings.Contains(r.RawQuery, "x-id=CreateBucket")
}

// GetObject reports whether r is a GetObject call (whole, ranged or by part).
func (r S3Request) GetObject() bool { return r.Method == http.MethodGet }

// S3Recorder is an SDK HTTP client that records every request it sends, so a
// test can assert which S3 operations a transfer used (#932).
type S3Recorder struct {
	base interface {
		Do(*http.Request) (*http.Response, error)
	}
	mu   sync.Mutex
	reqs []S3Request
}

// NewS3Recorder wraps base, or the SDK's default client when base is nil.
// Install it as aws.Config.HTTPClient or s3.Options.HTTPClient.
func NewS3Recorder(base interface {
	Do(*http.Request) (*http.Response, error)
}) *S3Recorder {
	if base == nil {
		base = awshttp.NewBuildableClient()
	}
	return &S3Recorder{base: base}
}

// Do implements the SDK's HTTPClient interface.
func (r *S3Recorder) Do(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, S3Request{req.Method, req.URL.RawQuery, req.Header.Get("Range"), req.Header.Clone()})
	r.mu.Unlock()
	return r.base.Do(req)
}

// Reset forgets every request recorded so far.
func (r *S3Recorder) Reset() {
	r.mu.Lock()
	r.reqs = nil
	r.mu.Unlock()
}

// Requests returns a copy of the requests recorded so far.
func (r *S3Recorder) Requests() []S3Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]S3Request(nil), r.reqs...)
}

// Count returns how many recorded requests satisfy keep.
func (r *S3Recorder) Count(keep func(S3Request) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, q := range r.reqs {
		if keep(q) {
			n++
		}
	}
	return n
}
