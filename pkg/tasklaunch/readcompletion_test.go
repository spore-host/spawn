package tasklaunch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// fakeCompletionStore is an offline CompletionStore holding objects by
// s3://bucket/key, and recording every read.
type fakeCompletionStore struct {
	account string
	objects map[string][]byte
	getErr  error // returned for every read when set
	reads   []string
}

func (f *fakeCompletionStore) GetAccountID(context.Context) (string, error) {
	return f.account, nil
}

func (f *fakeCompletionStore) GetS3Object(_ context.Context, _, bucket, key string) ([]byte, error) {
	uri := "s3://" + bucket + "/" + key
	f.reads = append(f.reads, uri)
	if f.getErr != nil {
		return nil, f.getErr
	}
	data, ok := f.objects[uri]
	if !ok {
		// What *aws.Client returns for a missing object.
		return nil, fmt.Errorf("%w: %s", aws.ErrS3NoSuchKey, uri)
	}
	return data, nil
}

const (
	rcAccount = "111122223333"
	rcRegion  = "us-east-1"
	rcTask    = "align-sample-42"
)

// The locations below are the documented contract, written out rather than
// derived: s3://spawn-results-<account>-<region>/tasks/<task_id>/completion.json
// by default (the key six adapter repos poll), and <results_prefix>/<task_id>/
// completion.json when the spec set one (spawn#646), which is where the wrapper
// writes it ("$RESULTS_PREFIX/completion.json").
func TestReadCompletion(t *testing.T) {
	const record = `{"task_id":"align-sample-42","run_id":"run-1","exit_code":0,"state":"completed",` +
		`"started_at":"2026-10-20T10:00:00Z","ended_at":"2026-10-20T10:05:00Z",` +
		`"instance_type":"m7i.large","purchase":"spot"}`

	cases := []struct {
		name          string
		resultsPrefix string
		objects       map[string]string
		wantURI       string
		wantErr       error // errors.Is target; nil means success
		wantAnyErr    bool  // an error that is NOT ErrNoCompletionRecord
		check         func(t *testing.T, rec *taskproto.CompletionRecord)
	}{
		{
			name:    "default prefix",
			objects: map[string]string{"s3://spawn-results-111122223333-us-east-1/tasks/align-sample-42/completion.json": record},
			wantURI: "s3://spawn-results-111122223333-us-east-1/tasks/align-sample-42/completion.json",
			check: func(t *testing.T, rec *taskproto.CompletionRecord) {
				if rec.State != taskproto.StateCompleted || rec.RunID != "run-1" {
					t.Errorf("record = %+v", rec)
				}
				if rec.InstanceType != "m7i.large" || rec.Purchase != taskproto.PurchaseSpot {
					t.Errorf("instance_type/purchase = %q/%q, want m7i.large/spot", rec.InstanceType, rec.Purchase)
				}
			},
		},
		{
			name:          "custom results_prefix",
			resultsPrefix: "s3://my-bucket/my-records/",
			objects: map[string]string{
				"s3://my-bucket/my-records/align-sample-42/completion.json": record,
				// A record at the default location must NOT be what is read.
				"s3://spawn-results-111122223333-us-east-1/tasks/align-sample-42/completion.json": `{"task_id":"wrong"}`,
			},
			wantURI: "s3://my-bucket/my-records/align-sample-42/completion.json",
			check: func(t *testing.T, rec *taskproto.CompletionRecord) {
				if rec.TaskID != rcTask {
					t.Errorf("read the record from the wrong place: task_id %q", rec.TaskID)
				}
			},
		},
		{
			name:    "missing record is the typed not-found",
			wantURI: "s3://spawn-results-111122223333-us-east-1/tasks/align-sample-42/completion.json",
			wantErr: ErrNoCompletionRecord,
		},
		{
			name:       "malformed record is an error, not not-found",
			objects:    map[string]string{"s3://spawn-results-111122223333-us-east-1/tasks/align-sample-42/completion.json": `{"task_id": "align-sample-42", "exit_code": `},
			wantURI:    "s3://spawn-results-111122223333-us-east-1/tasks/align-sample-42/completion.json",
			wantAnyErr: true,
		},
		{
			name: "old record without instance_type or purchase still parses",
			objects: map[string]string{"s3://spawn-results-111122223333-us-east-1/tasks/align-sample-42/completion.json": `{"task_id":"align-sample-42","exit_code":3,"state":"failed",` +
				`"started_at":"2026-01-01T00:00:00Z","ended_at":"2026-01-01T00:01:00Z","logs":[],"retry_class":"app_error"}`},
			wantURI: "s3://spawn-results-111122223333-us-east-1/tasks/align-sample-42/completion.json",
			check: func(t *testing.T, rec *taskproto.CompletionRecord) {
				if rec.ExitCode != 3 || rec.RetryClass != taskproto.RetryAppError {
					t.Errorf("record = %+v", rec)
				}
				if rec.InstanceType != "" || rec.Purchase != "" {
					t.Errorf("an old record grew instance facts: %q/%q", rec.InstanceType, rec.Purchase)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCompletionStore{account: rcAccount, objects: map[string][]byte{}}
			for k, v := range tc.objects {
				store.objects[k] = []byte(v)
			}
			rec, uri, err := ReadCompletion(context.Background(), store, rcTask, tc.resultsPrefix, rcRegion)
			if uri != tc.wantURI {
				t.Errorf("uri = %q, want %q", uri, tc.wantURI)
			}
			if len(store.reads) != 1 || store.reads[0] != tc.wantURI {
				t.Errorf("reads = %v, want exactly [%s]", store.reads, tc.wantURI)
			}
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
				if rec != nil {
					t.Errorf("rec = %+v with a not-found error", rec)
				}
			case tc.wantAnyErr:
				if err == nil {
					t.Fatalf("err = nil, rec = %+v; want an error", rec)
				}
				if errors.Is(err, ErrNoCompletionRecord) {
					t.Errorf("a malformed record was reported as absent: %v", err)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				tc.check(t, rec)
			}
		})
	}
}

// A failed read is a failure, never "no record yet": treating an AccessDenied
// as absent would report a finished task as still running.
func TestReadCompletion_ReadFailureIsNotNotFound(t *testing.T) {
	store := &fakeCompletionStore{account: rcAccount, getErr: errors.New("AccessDenied")}
	_, _, err := ReadCompletion(context.Background(), store, rcTask, "", rcRegion)
	if err == nil || errors.Is(err, ErrNoCompletionRecord) {
		t.Fatalf("err = %v; want a real error, not ErrNoCompletionRecord", err)
	}
}

// A results_prefix that is not an s3:// URI is an error before any read.
func TestReadCompletion_BadResultsPrefix(t *testing.T) {
	store := &fakeCompletionStore{account: rcAccount}
	_, _, err := ReadCompletion(context.Background(), store, rcTask, "my-bucket/records", rcRegion)
	if err == nil || errors.Is(err, ErrNoCompletionRecord) {
		t.Fatalf("err = %v; want a bad-prefix error", err)
	}
	if len(store.reads) != 0 {
		t.Errorf("read %v with an unusable prefix", store.reads)
	}
}
