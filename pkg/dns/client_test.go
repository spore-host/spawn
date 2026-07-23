package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestClientGetFQDN(t *testing.T) {
	c := &Client{domain: "example.com"}
	if got := c.GetFQDN("my-instance"); got != "my-instance.example.com" {
		t.Errorf("Client.GetFQDN = %q, want my-instance.example.com", got)
	}
}

func TestCallAPI_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %q", ct)
		}
		// Echo back a success response.
		_ = json.NewEncoder(w).Encode(DNSUpdateResponse{
			Success:  true,
			Message:  "ok",
			Record:   "my-instance.spore.host",
			ChangeID: "C123",
		})
	}))
	defer ts.Close()

	c := &Client{httpClient: ts.Client(), apiEndpoint: ts.URL, domain: "spore.host"}
	resp, err := c.callAPI(context.Background(), DNSUpdateRequest{RecordName: "my-instance", Action: "UPSERT"})
	if err != nil {
		t.Fatalf("callAPI success: %v", err)
	}
	if !resp.Success || resp.ChangeID != "C123" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestCallAPI_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DNSUpdateResponse{
			Success: false,
			Error:   "record already exists",
		})
	}))
	defer ts.Close()

	c := &Client{httpClient: ts.Client(), apiEndpoint: ts.URL, domain: "spore.host"}
	resp, err := c.callAPI(context.Background(), DNSUpdateRequest{RecordName: "x", Action: "UPSERT"})
	if err == nil {
		t.Fatal("expected error when Success=false")
	}
	if resp == nil || !strings.Contains(err.Error(), "record already exists") {
		t.Errorf("expected API error surfaced, got resp=%+v err=%v", resp, err)
	}
}

// TestCallAPI_ForbiddenNotSwallowed guards the #435 fix: a Function URL under
// AuthType: AWS_IAM rejects a bad/absent SigV4 signature with a 403 whose body is
// AWS's own JSON (`{"Message":"Forbidden"}`). That unmarshals cleanly into an
// all-empty DNSUpdateResponse (Success=false, Error=""), which previously produced
// a useless `DNS API error: ` (empty) and hid the real cause. The status check must
// surface the 403 and body instead.
func TestCallAPI_ForbiddenNotSwallowed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"Message":"Forbidden"}`))
	}))
	defer ts.Close()

	c := &Client{httpClient: ts.Client(), apiEndpoint: ts.URL, domain: "spore.host"}
	_, err := c.callAPI(context.Background(), DNSUpdateRequest{RecordName: "x", Action: "UPSERT"})
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("403 must be surfaced with status and body, got: %v", err)
	}
}

func TestCallAPI_MalformedResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer ts.Close()

	c := &Client{httpClient: ts.Client(), apiEndpoint: ts.URL, domain: "spore.host"}
	if _, err := c.callAPI(context.Background(), DNSUpdateRequest{}); err == nil {
		t.Error("expected parse error for malformed response")
	}
}

func TestCallAPI_UnreachableEndpoint(t *testing.T) {
	c := &Client{httpClient: http.DefaultClient, apiEndpoint: "http://127.0.0.1:0/invalid", domain: "spore.host"}
	if _, err := c.callAPI(context.Background(), DNSUpdateRequest{}); err == nil {
		t.Error("expected transport error for unreachable endpoint")
	}
}

func TestRegisterDNS_InvalidName(t *testing.T) {
	// Validation happens before any IMDS/HTTP call, so a nil imdsClient is fine.
	// Note: names are lowercased before the regex, so "UPPER" becomes valid —
	// only characters outside [a-z0-9-] reject. Use those.
	c := &Client{domain: "spore.host"}
	for _, bad := range []string{"has spaces", "under_score", "bad!char", "dot.ted", ""} {
		if _, err := c.RegisterDNS(context.Background(), bad, "1.2.3.4"); err == nil {
			t.Errorf("RegisterDNS(%q) should reject invalid name", bad)
		}
	}
}

func TestRegisterJobArrayDNS_InvalidName(t *testing.T) {
	c := &Client{domain: "spore.host"}
	// Job-array names allow dots but still reject spaces/underscores/bang.
	for _, bad := range []string{"has spaces", "under_score", "bad!char"} {
		if _, err := c.RegisterJobArrayDNS(context.Background(), bad, "1.2.3.4", "ja-1", "myarray"); err == nil {
			t.Errorf("RegisterJobArrayDNS(%q) should reject invalid name", bad)
		}
	}
}

// TestCallAPI_UnsignedByDefault verifies that with signing OFF (the default,
// pre-#173-cutover state) the request carries NO SigV4 Authorization header — so
// the signing build is non-breaking against the current AuthType: NONE Function
// URL.
func TestCallAPI_UnsignedByDefault(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(DNSUpdateResponse{Success: true, ChangeID: "C1"})
	}))
	defer ts.Close()

	c := &Client{httpClient: ts.Client(), apiEndpoint: ts.URL, domain: "spore.host"}
	if _, err := c.callAPI(context.Background(), DNSUpdateRequest{RecordName: "x", Action: "UPSERT"}); err != nil {
		t.Fatalf("callAPI: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("unsigned client sent Authorization header %q, want none", gotAuth)
	}
}

// TestCallAPI_SignedWhenEnabled verifies that with signing ON (#173) the request
// carries a SigV4 Authorization header scoped to the lambda service — the header
// the Function URL needs once it runs under AuthType: AWS_IAM.
func TestCallAPI_SignedWhenEnabled(t *testing.T) {
	var gotAuth, gotDate string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("X-Amz-Date")
		_ = json.NewEncoder(w).Encode(DNSUpdateResponse{Success: true, ChangeID: "C1"})
	}))
	defer ts.Close()

	c := &Client{
		httpClient:    ts.Client(),
		apiEndpoint:   ts.URL,
		domain:        "spore.host",
		sign:          true,
		region:        "us-east-1",
		signer:        v4.NewSigner(),
		credsProvider: credentials.NewStaticCredentialsProvider("AKIAEXAMPLE", "secretkeyexample", ""),
	}
	if _, err := c.callAPI(context.Background(), DNSUpdateRequest{RecordName: "x", Action: "UPSERT"}); err != nil {
		t.Fatalf("callAPI: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization = %q, want an AWS4-HMAC-SHA256 SigV4 header", gotAuth)
	}
	if !strings.Contains(gotAuth, "/us-east-1/lambda/aws4_request") {
		t.Errorf("Authorization credential scope = %q, want .../us-east-1/lambda/aws4_request", gotAuth)
	}
	if gotDate == "" {
		t.Error("signed request missing X-Amz-Date header")
	}
}

// TestSignRequest_NoOpWhenDisabled asserts signRequest leaves the request
// untouched when signing is off, even if a (here nil) signer would otherwise be
// invoked — the guard that keeps the unsigned path safe.
func TestSignRequest_NoOpWhenDisabled(t *testing.T) {
	c := &Client{} // sign=false, signer=nil
	req, _ := http.NewRequest("POST", "https://example.test", strings.NewReader("{}"))
	if err := c.signRequest(context.Background(), req, []byte("{}")); err != nil {
		t.Fatalf("signRequest disabled should be a no-op, got %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("signRequest disabled must not add an Authorization header")
	}
}

var _ aws.CredentialsProvider = credentials.StaticCredentialsProvider{}

// TestSetAccountName_SentInRequest verifies SetAccountName makes callAPI include
// account_name in the JSON body — the field the dns-updater uses to register the
// friendly alias FQDN (#121 / spore-host#357).
func TestSetAccountName_SentInRequest(t *testing.T) {
	var gotBody DNSUpdateRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(DNSUpdateResponse{Success: true, ChangeID: "C1"})
	}))
	defer ts.Close()

	c := &Client{httpClient: ts.Client(), apiEndpoint: ts.URL, domain: "spore.host"}
	c.SetAccountName("mycelium-development")
	if c.accountName != "mycelium-development" {
		t.Fatalf("SetAccountName didn't set the field: %q", c.accountName)
	}

	// callAPI sends whatever the caller put in the request; the production
	// RegisterDNS path copies c.accountName into AccountName. Simulate that.
	if _, err := c.callAPI(context.Background(), DNSUpdateRequest{
		RecordName:  "job",
		Action:      "UPSERT",
		AccountName: c.accountName,
	}); err != nil {
		t.Fatalf("callAPI: %v", err)
	}
	if gotBody.AccountName != "mycelium-development" {
		t.Errorf("server received account_name=%q, want mycelium-development", gotBody.AccountName)
	}
}
