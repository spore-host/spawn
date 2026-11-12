package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeRegistry substitutes the portal-registry lookup.
type fakeRegistry struct {
	mu    sync.Mutex
	rows  map[string]string // accountId → status
	err   error
	calls int
}

func (f *fakeRegistry) lookup(_ context.Context, _ aws.Config, id string) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return false, "", f.err
	}
	st, ok := f.rows[id]
	return ok, st, nil
}

func useRegistry(t *testing.T, f *fakeRegistry, allow string) {
	t.Helper()
	orig := portalAccountLookup
	portalAccountLookup = f.lookup
	t.Setenv("DASHBOARD_ALLOWED_ACCOUNTS", allow)
	resetEntitlementCache()
	t.Cleanup(func() { portalAccountLookup = orig; resetEntitlementCache() })
}

func TestAuthorizeAccount(t *testing.T) {
	ctx := context.Background()
	reg := &fakeRegistry{rows: map[string]string{
		"111111111111": "active",
		"222222222222": "dormant",
		"333333333333": "offboarded",
		"444444444444": "",
	}}
	useRegistry(t, reg, " 555555555555 , not-an-id")

	for id, want := range map[string]error{
		"111111111111": nil,            // onboarded
		"222222222222": nil,            // dormant is still a customer
		"444444444444": nil,            // a row with no status yet
		"555555555555": nil,            // allow-listed
		"333333333333": errNotEntitled, // offboarded
		"999999999999": errNotEntitled, // a stranger
		"not-an-id":    errNotEntitled, // the malformed allow-list entry entitles nothing
		"":             errNotEntitled,
	} {
		if err := authorizeAccount(ctx, aws.Config{}, id); !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Errorf("authorizeAccount(%q) = %v, want %v", id, err, want)
		}
	}

	// Answers are cached, refusals too (briefly).
	n := reg.calls
	_ = authorizeAccount(ctx, aws.Config{}, "111111111111")
	_ = authorizeAccount(ctx, aws.Config{}, "999999999999")
	if reg.calls != n {
		t.Errorf("cached answers re-read the registry (%d -> %d)", n, reg.calls)
	}
}

// An unreadable registry refuses, and is not cached.
func TestAuthorizeAccountFailsClosed(t *testing.T) {
	ctx := context.Background()
	reg := &fakeRegistry{err: errors.New("AccessDeniedException")}
	useRegistry(t, reg, "")
	if err := authorizeAccount(ctx, aws.Config{}, "111111111111"); !errors.Is(err, errEntitlementUnavailable) {
		t.Fatalf("err = %v, want errEntitlementUnavailable", err)
	}
	reg.err, reg.rows = nil, map[string]string{"111111111111": "active"}
	if err := authorizeAccount(ctx, aws.Config{}, "111111111111"); err != nil {
		t.Errorf("a failed lookup was cached as a refusal: %v", err)
	}

	// Nothing named: refused, not waved through.
	if err := authorizeAccounts(ctx, aws.Config{}, "", ""); !errors.Is(err, errNotEntitled) {
		t.Errorf("authorizeAccounts with no account = %v, want errNotEntitled", err)
	}
	// Every named account must pass.
	if err := authorizeAccounts(ctx, aws.Config{}, "111111111111", "999999999999"); !errors.Is(err, errNotEntitled) {
		t.Errorf("one stranger among the accounts = %v, want errNotEntitled", err)
	}
}

func TestAccountOfScope(t *testing.T) {
	for in, want := range map[string]string{
		"111111111111":                               "111111111111",
		"arn:aws:iam::111111111111:user/alice":       "111111111111",
		"arn:aws:sts::111111111111:assumed-role/r/s": "111111111111",
		"arn:aws:iam::abc:user/alice":                "",
		"alice":                                      "",
		"":                                           "",
	} {
		if got := accountOfScope(in); got != want {
			t.Errorf("accountOfScope(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeEntitlementAWS answers STS GetCallerIdentity as arn, and records every other call:
// a refused caller must cause none (no cache row, no table, no S3).
type fakeEntitlementAWS struct {
	mu    sync.Mutex
	arn   string
	other []string
}

func (f *fakeEntitlementAWS) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "Action=GetCallerIdentity") {
			acct := strings.Split(f.arn, ":")[4]
			w.Header().Set("Content-Type", "text/xml")
			_, _ = fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>%s</Arn><UserId>AID</UserId><Account>%s</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`, f.arn, acct) // nosemgrep
			return
		}
		f.mu.Lock()
		f.other = append(f.other, r.Header.Get("X-Amz-Target")+" "+r.URL.Path)
		f.mu.Unlock()
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func credentialedRequest(method, path, body string) events.APIGatewayProxyRequest {
	creds := base64.StdEncoding.EncodeToString([]byte(`{"accessKeyId":"AKIDEXAMPLE","secretAccessKey":"secret","sessionToken":"tok"}`))
	return events.APIGatewayProxyRequest{
		HTTPMethod: method,
		Path:       path,
		Body:       body,
		Headers:    map[string]string{"x-aws-credentials": creds, "content-type": "application/json"},
	}
}

// Through the handler: a stranger with valid credentials, including one
// assuming a role named spore-portal-launch in their own account, is refused
// before any route runs and before anything is written.
func TestHandlerRefusesUnentitledAccounts(t *testing.T) {
	reg := &fakeRegistry{rows: map[string]string{"111111111111": "active"}}
	useRegistry(t, reg, "")

	for _, arn := range []string{
		"arn:aws:iam::999999999999:user/stranger",
		"arn:aws:sts::999999999999:assumed-role/spore-portal-launch/s",
	} {
		for _, rt := range []struct{ method, path, body string }{
			{"POST", "/teams", `{"name":"x"}`},
			{"POST", "/teams/t1/members", `{"member_arn":"arn:aws:iam::1:user/x"}`},
			{"POST", "/api/strata/resolve", `{"formation":"r-research@2024.03"}`},
			{"GET", "/api/instances", ""},
		} {
			t.Run(arn+" "+rt.method+" "+rt.path, func(t *testing.T) {
				fa := &fakeEntitlementAWS{arn: arn}
				useFakeAWS(t, fa.server(t).URL)
				resp, err := handler(context.Background(), credentialedRequest(rt.method, rt.path, rt.body))
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("status %d (%s), want 403", resp.StatusCode, resp.Body)
				}
				if len(fa.other) != 0 {
					t.Errorf("a refused caller caused AWS calls: %v", fa.other)
				}
			})
		}
	}

	// The same portal role in an onboarded account gets through to the route
	// (here a 400 for a bad body, which needs no AWS).
	fa := &fakeEntitlementAWS{arn: "arn:aws:sts::111111111111:assumed-role/spore-portal-launch/s"}
	useFakeAWS(t, fa.server(t).URL)
	resp, _ := handler(context.Background(), credentialedRequest("POST", "/api/strata/resolve", `{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("onboarded portal caller: status %d (%s), want the route's 400", resp.StatusCode, resp.Body)
	}

	// A registry that cannot be read refuses with 503.
	reg.err = errors.New("throttled")
	resetEntitlementCache()
	resp, _ = handler(context.Background(), credentialedRequest("POST", "/api/strata/resolve", `{}`))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unreadable registry: status %d, want 503", resp.StatusCode)
	}
}

func useFakeAWS(t *testing.T, url string) {
	t.Helper()
	orig := loadAWSConfig
	t.Cleanup(func() { loadAWSConfig = orig })
	loadAWSConfig = func(context.Context) (aws.Config, error) {
		return aws.Config{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(url),
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: "L", SecretAccessKey: "L"}, nil
			}),
		}, nil
	}
}
