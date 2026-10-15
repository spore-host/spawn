package testutil

import (
	"net/http"
	"regexp"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// SignedRequest is one request a SigningRecorder carried.
type SignedRequest struct {
	Method, Path string
	// Service and Region are the SigV4 credential scope; "" when unsigned.
	Service, Region string
}

// SigningRecorder is an aws.HTTPClient that records the SigV4 credential scope
// of every request it forwards.
//
// The emulator answers whatever region a request is signed for, so a client in
// the wrong region looks fine against it. Real S3 does not: a request signed
// for one region against a bucket in another gets a 301 / PermanentRedirect.
// The signing region is the property to assert, and this exposes it.
type SigningRecorder struct {
	inner aws.HTTPClient
	mu    sync.Mutex
	reqs  []SignedRequest
}

var sigV4Scope = regexp.MustCompile(`Credential=[^/]+/\d{8}/([^/]+)/([^/]+)/aws4_request`)

// RecordSigning returns a copy of cfg whose requests pass through a new
// SigningRecorder, and the recorder.
func RecordSigning(cfg aws.Config) (aws.Config, *SigningRecorder) {
	out := cfg.Copy()
	rec := &SigningRecorder{inner: out.HTTPClient}
	if rec.inner == nil {
		rec.inner = http.DefaultClient
	}
	out.HTTPClient = rec
	return out, rec
}

// Do implements aws.HTTPClient.
func (r *SigningRecorder) Do(req *http.Request) (*http.Response, error) {
	sr := SignedRequest{Method: req.Method, Path: req.URL.Path}
	if m := sigV4Scope.FindStringSubmatch(req.Header.Get("Authorization")); m != nil {
		sr.Region, sr.Service = m[1], m[2]
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, sr)
	r.mu.Unlock()
	return r.inner.Do(req)
}

// Requests returns the requests recorded for service ("s3", "dynamodb", ...).
func (r *SigningRecorder) Requests(service string) []SignedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SignedRequest
	for _, sr := range r.reqs {
		if sr.Service == service {
			out = append(out, sr)
		}
	}
	return out
}
