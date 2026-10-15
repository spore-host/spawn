package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/spore-host/spawn/pkg/queue"
)

// countingTransport fails every request without touching the network, and
// counts them, so a test can tell whether an upload was attempted.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("test transport: no network")
}

// A result-path pattern is validated before globbing, but the files the glob
// then matches were not: a wildcard through a symlink under an allowed
// directory ("<results>/l*/hosts" with link -> /etc) passed the pattern check
// and uploaded the system file. Every match must be validated (#1000).
func TestUploadJobResults_ValidatesEachGlobMatch(t *testing.T) {
	sys := "/etc"
	if os.PathSeparator == '\\' {
		sys = os.Getenv("SystemRoot")
		if sys == "" {
			sys = `C:\Windows`
		}
	}
	victim := "hosts"
	if os.PathSeparator == '\\' {
		victim = "win.ini"
	}
	if _, err := os.Stat(filepath.Join(sys, victim)); err != nil {
		t.Skipf("%s not present: %v", filepath.Join(sys, victim), err)
	}
	dir := t.TempDir()
	if err := os.Symlink(sys, filepath.Join(dir, "link")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	tr := &countingTransport{}
	client := s3.New(s3.Options{
		Region:      "us-east-1",
		Credentials: aws.AnonymousCredentials{},
		HTTPClient:  &http.Client{Transport: tr},
	})
	r := &QueueRunner{
		config:     &queue.QueueConfig{ResultS3Bucket: "results", ResultS3Prefix: "q"},
		s3Uploader: manager.NewUploader(client),
		ctx:        context.Background(),
	}

	pattern := filepath.Join(dir, "l*", victim)
	err := r.uploadJobResults("job-1", []string{pattern})
	if err == nil || !strings.Contains(err.Error(), "system paths") {
		t.Fatalf("uploadJobResults(%q) error = %v, want a refusal of the system file it matches", pattern, err)
	}
	if got := tr.n.Load(); got != 0 {
		t.Fatalf("%d upload request(s) were attempted for a refused path", got)
	}
}
