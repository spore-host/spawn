package dns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The client must work where there is no instance metadata service (#872).
// `spawn dns register` and `spawn dns delete` run on a workstation; the client
// used to fetch the instance identity document from IMDS before every request —
// fields the updater has ignored since #173 — so both commands failed there
// before sending anything.
//
// Built through NewClient with IMDS disabled, so the test sees what a
// workstation sees, and run against every request the client can make.
func TestEveryRequestWorksWithoutIMDS(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("SPORE_DNS_SIGV4", "0") // signing is covered in signing_test.go; no credentials here

	var bodies []map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		_, _ = w.Write([]byte(`{"success":true,"record":"x.spore.host"}`))
	}))
	defer ts.Close()

	c, err := NewClient(context.Background(), "", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	calls := map[string]func() error{
		"RegisterDNS": func() error { _, err := c.RegisterDNS(ctx, "web", "203.0.113.7"); return err },
		"DeleteDNS":   func() error { _, err := c.DeleteDNS(ctx, "web", "203.0.113.7"); return err },
		"RegisterJobArrayDNS": func() error {
			_, err := c.RegisterJobArrayDNS(ctx, "web-0", "203.0.113.7", "ja-1", "web")
			return err
		},
		"DeleteJobArrayDNS": func() error {
			_, err := c.DeleteJobArrayDNS(ctx, "web-0", "203.0.113.7", "ja-1", "web")
			return err
		},
	}
	for name, call := range calls {
		if err := call(); err != nil {
			t.Errorf("%s failed with no IMDS — `spawn dns` cannot work from a workstation: %v", name, err)
		}
	}
	if len(bodies) != len(calls) {
		t.Fatalf("the server saw %d requests, want %d", len(bodies), len(calls))
	}
	for _, m := range bodies {
		for k := range m {
			if strings.HasPrefix(k, "instance_identity") {
				t.Errorf("a request still carries %q, which the updater ignores", k)
			}
		}
	}
}
