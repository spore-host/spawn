package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeCacheAWS answers STS GetCallerIdentity as arn, and answers every
// spawn-user-accounts GetItem with a row mapping the caller to a victim's CLI
// identity: the state #1589 describes, a row once written for the shared
// Cognito session ARN. It records every DynamoDB call.
type fakeCacheAWS struct {
	mu    sync.Mutex
	arn   string
	calls []string
}

const victimCliArn = "arn:aws:iam::111111111111:user/victim"

func (f *fakeCacheAWS) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "Action=GetCallerIdentity") {
			acct := strings.Split(f.arn, ":")[4]
			w.Header().Set("Content-Type", "text/xml")
			_, _ = fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>%s</Arn><UserId>AID</UserId><Account>%s</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`, f.arn, acct) // nosemgrep
			return
		}
		target := r.Header.Get("X-Amz-Target")
		f.mu.Lock()
		f.calls = append(f.calls, target)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		switch target {
		case "DynamoDB_20120810.GetItem":
			_, _ = fmt.Fprintf(w, `{"Item":{"user_id":{"S":%q},"cli_iam_arn":{"S":%q},"aws_account_id":{"S":"111111111111"},"account_base36":{"S":"x"}}}`, f.arn, victimCliArn) // nosemgrep
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeCacheAWS) called(target string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == target {
			return true
		}
	}
	return false
}

func fakeCacheConfig(url string) aws.Config {
	return aws.Config{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(url),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "L", SecretAccessKey: "L"}, nil
		}),
	}
}

// An identity-pool caller's ARN is shared by every web user, so it must never
// key the cache (#1589): no GetItem, no PutItem, and with no verified email
// claim the caller resolves to nobody, not to the row's victim.
func TestAssumedRoleSessionNeverUsesTheCache(t *testing.T) {
	useRegistry(t, &fakeRegistry{rows: map[string]string{"111111111111": "active", "222222222222": "active"}}, "")
	shared := "arn:aws:sts::222222222222:assumed-role/Cognito_SpawnDashboard_Auth_Role/CognitoIdentityCredentials"

	fa := &fakeCacheAWS{arn: shared}
	cfg := fakeCacheConfig(fa.server(t).URL)
	req := credentialedRequest("GET", "/api/sweeps", "")
	_, scope, _, _, err := getUserFromCredentialsHeader(context.Background(), cfg, req, req.Headers["x-aws-credentials"])
	if err == nil || scope == victimCliArn {
		t.Fatalf("shared Cognito session resolved to scope %q (err %v); want refusal for want of an email claim", scope, err)
	}
	for _, target := range []string{"DynamoDB_20120810.GetItem", "DynamoDB_20120810.PutItem"} {
		if fa.called(target) {
			t.Errorf("assumed-role session caused %s on the user-account cache", target)
		}
	}

	// The IAM-auth path, too.
	req2 := credentialedRequest("GET", "/api/sweeps", "")
	req2.Headers = map[string]string{}
	req2.RequestContext.Identity.UserArn = shared
	req2.RequestContext.Identity.AccountID = "222222222222"
	fb := &fakeCacheAWS{arn: shared}
	cfg2 := fakeCacheConfig(fb.server(t).URL)
	_, scope, _, _, err = getUserFromRequest(context.Background(), cfg2, req2)
	if err == nil || scope == victimCliArn {
		t.Fatalf("IAM-auth assumed-role session resolved to scope %q (err %v); want refusal", scope, err)
	}
	if fb.called("DynamoDB_20120810.GetItem") || fb.called("DynamoDB_20120810.PutItem") {
		t.Errorf("IAM-auth assumed-role session used the cache: %v", fb.calls)
	}
}

// The contrast that keeps the test above meaningful: an IAM user's ARN is
// per-person, and still reads the cache.
func TestIAMUserStillUsesTheCache(t *testing.T) {
	useRegistry(t, &fakeRegistry{rows: map[string]string{"111111111111": "active"}}, "")
	fa := &fakeCacheAWS{arn: "arn:aws:iam::111111111111:user/victim"}
	cfg := fakeCacheConfig(fa.server(t).URL)
	req := credentialedRequest("GET", "/api/sweeps", "")
	_, scope, _, _, err := getUserFromCredentialsHeader(context.Background(), cfg, req, req.Headers["x-aws-credentials"])
	if err != nil || scope != victimCliArn {
		t.Fatalf("scope %q err %v, want the cached %q", scope, err, victimCliArn)
	}
	if !fa.called("DynamoDB_20120810.GetItem") {
		t.Fatal("an IAM user's request did not read the cache")
	}
}

func TestIsAssumedRoleSession(t *testing.T) {
	for in, want := range map[string]bool{
		"arn:aws:sts::222222222222:assumed-role/Cognito_SpawnDashboard_Auth_Role/CognitoIdentityCredentials": true,
		"arn:aws-us-gov:sts::222222222222:assumed-role/r/s":                                                  true,
		"arn:aws:iam::111111111111:user/alice":                                                               false,
		"us-east-1:1234-cognito-sub":                                                                         false,
		"":                                                                                                   false,
	} {
		if got := isAssumedRoleSession(in); got != want {
			t.Errorf("isAssumedRoleSession(%q) = %v, want %v", in, got, want)
		}
	}
}
