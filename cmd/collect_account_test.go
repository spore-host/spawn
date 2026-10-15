package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/sweep"
)

const (
	collectTestInfraKey   = "AKIDINFRAACCOUNT"
	collectTestComputeKey = "AKIDCOMPUTEACCOUNT"
	collectTestComputeAcc = "435415984226"
)

// fakeResultsS3 answers ListObjectsV2 with one result object and GetObject with
// a result body, recording the access key that signed each request.
type fakeResultsS3 struct {
	mu   sync.Mutex
	keys []string // "<METHOD> <path> <access key>"
}

var credentialRE = regexp.MustCompile(`Credential=([^/]+)/`)

func (f *fakeResultsS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := ""
	if m := credentialRE.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		key = m[1]
	}
	f.mu.Lock()
	f.keys = append(f.keys, fmt.Sprintf("%s %s %s", r.Method, r.URL.Path, key))
	f.mu.Unlock()

	if r.URL.Query().Get("list-type") == "2" {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <IsTruncated>false</IsTruncated>
  <KeyCount>1</KeyCount>
  <Contents><Key>sweeps/sweep-x/0/results.json</Key><Size>20</Size></Contents>
</ListBucketResult>`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"accuracy": 0.9}`)
}

func staticConfig(endpoint, key string) aws.Config {
	return aws.Config{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(key, "secret", ""),
		BaseEndpoint: aws.String(endpoint),
	}
}

// stubCollectAccounts points collect-results' credential seams at srv: the infra
// config signs with collectTestInfraKey, the compute config with
// collectTestComputeKey, and the compute config resolves to computeAccount.
func stubCollectAccounts(t *testing.T, srv *httptest.Server, rec *sweep.SweepRecord, computeAccount string) {
	t.Helper()
	oldInfra, oldCompute, oldGet, oldAcct := collectInfraConfig, collectComputeConfig, collectGetSweep, collectAccountOf
	oldID, oldOut, oldFmt, oldPrefix, oldRegions := collectSweepID, collectOutputFile, collectFormat, collectS3Prefix, collectRegions
	t.Cleanup(func() {
		collectInfraConfig, collectComputeConfig, collectGetSweep, collectAccountOf = oldInfra, oldCompute, oldGet, oldAcct
		collectSweepID, collectOutputFile, collectFormat, collectS3Prefix, collectRegions = oldID, oldOut, oldFmt, oldPrefix, oldRegions
	})

	collectInfraConfig = func(context.Context, string) (aws.Config, error) {
		return staticConfig(srv.URL, collectTestInfraKey), nil
	}
	collectComputeConfig = func(_ context.Context, region string) (aws.Config, error) {
		c := staticConfig(srv.URL, collectTestComputeKey)
		c.Region = region
		return c, nil
	}
	collectGetSweep = func(_ context.Context, cfg aws.Config, id string) (*sweep.SweepRecord, error) {
		creds, err := cfg.Credentials.Retrieve(context.Background())
		if err != nil || creds.AccessKeyID != collectTestInfraKey {
			t.Errorf("sweep record read with %q, want the infra credentials (the table is there, #326)", creds.AccessKeyID)
		}
		return rec, nil
	}
	collectAccountOf = func(_ context.Context, cfg aws.Config) (string, error) {
		creds, _ := cfg.Credentials.Retrieve(context.Background())
		if creds.AccessKeyID == collectTestComputeKey {
			return computeAccount, nil
		}
		return "966362334030", nil
	}

	collectSweepID = rec.SweepID
	collectOutputFile = filepath.Join(t.TempDir(), "results.json")
	collectFormat = "json"
	collectS3Prefix = ""
	collectRegions = nil
}

// #1034: spawn-results-<account>-<region> belongs to the sweep's compute account
// (named for aws_account_id, written by instances running there, created only
// with compute credentials, no bucket policy for anyone else). Every S3 request
// collect makes must therefore be signed by the compute credentials. The infra
// credentials, which the sweep record is read with, get AccessDenied, and that
// used to end in "No results found" and exit 0.
func TestCollectResultsReadsResultsWithTheComputeAccount(t *testing.T) {
	fake := &fakeResultsS3{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	rec := &sweep.SweepRecord{SweepID: "sweep-x", Region: "us-west-2", AWSAccountID: collectTestComputeAcc}
	stubCollectAccounts(t, srv, rec, collectTestComputeAcc)

	if err := runCollectResults(collectCmd, nil); err != nil {
		t.Fatalf("runCollectResults: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.keys) == 0 {
		t.Fatal("no S3 request was made")
	}
	for _, k := range fake.keys {
		if !strings.HasSuffix(k, " "+collectTestComputeKey) {
			t.Errorf("S3 request %q was not signed by the compute account's credentials", k)
		}
		if !strings.Contains(k, "/spawn-results-"+collectTestComputeAcc+"-us-west-2") {
			t.Errorf("S3 request %q is not to the sweep account's results bucket", k)
		}
	}

	data, err := os.ReadFile(collectOutputFile)
	if err != nil {
		t.Fatalf("no output written: %v", err)
	}
	var got []SweepResult
	if err := json.Unmarshal(data, &got); err != nil || len(got) != 1 {
		t.Errorf("output = %s (err %v), want the one result", data, err)
	}
}

// Compute credentials for a different account cannot read the sweep's buckets
// either. That must be an error naming both accounts, not an empty collection.
func TestCollectResultsRefusesComputeCredentialsForAnotherAccount(t *testing.T) {
	fake := &fakeResultsS3{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	rec := &sweep.SweepRecord{SweepID: "sweep-x", Region: "us-west-2", AWSAccountID: collectTestComputeAcc}
	stubCollectAccounts(t, srv, rec, "111111111111")

	err := runCollectResults(collectCmd, nil)
	if err == nil {
		t.Fatal("runCollectResults succeeded with compute credentials for the wrong account")
	}
	for _, want := range []string{collectTestComputeAcc, "111111111111"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name account %s", err, want)
		}
	}
	if len(fake.keys) != 0 {
		t.Errorf("S3 was called anyway: %v", fake.keys)
	}
}
