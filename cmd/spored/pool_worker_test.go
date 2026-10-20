package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestNewPoolWorker_WiresTheResultsWriter: the worker writes the failed
// completion record for an unparseable spec itself (#1149), but only through
// Worker.Results. A production worker built without it logs and acks with no
// record, which is the bug: `spawn task status` reports the task unknown forever.
func TestNewPoolWorker_WiresTheResultsWriter(t *testing.T) {
	client := s3.New(s3.Options{Region: "us-west-2"})
	w := newPoolWorker(nil, client, "123456789012", "us-west-2", poolWorkerOpts{runID: "r", specBucket: "b"})
	if w.Results == nil {
		t.Fatal("pool worker has no Results writer; an unparseable spec gets no completion record")
	}
	if w.AccountID != "123456789012" || w.Region != "us-west-2" {
		t.Errorf("AccountID/Region = %q/%q; the default results prefix cannot be located", w.AccountID, w.Region)
	}
}
