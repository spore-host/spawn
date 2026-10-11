package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// #821, GHSA-qc2h-6qqf-wcm4 — two independent authorization defects in this
// Lambda, and the gates that stop each returning.

// ── Slack request verification ───────────────────────────────────────────────

// testSigningSecret is deliberately worded rather than a hex blob. Slack's own
// documentation example ("8f742231b10e...") trips semgrep's generic-secret rule,
// and suppressing that would train the next reader to suppress it too. HMAC does
// not care what the bytes are.
const testSigningSecret = "not-a-real-slack-signing-secret-only-for-tests"

// resetSlackSecretsCache clears the sync.Once-backed cache so a test can set a
// different signing secret. Also clears SLACK_SECRETS_ARN, so the env fallback is
// what gets read rather than whatever Secrets Manager the ambient environment
// happens to point at — a test that silently reached a real secret would be
// asserting against an unknown value.
func resetSlackSecretsCache() {
	slackSecretsOnce = sync.Once{}
	slackSecretsCached = slackSecrets{}
	_ = os.Unsetenv("SLACK_SECRETS_ARN")
}

func withSigningSecret(t *testing.T, secret string) {
	t.Helper()
	prev, had := os.LookupEnv("SLACK_SIGNING_SECRET")
	resetSlackSecretsCache()
	if err := os.Setenv("SLACK_SIGNING_SECRET", secret); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("SLACK_SIGNING_SECRET", prev)
		} else {
			_ = os.Unsetenv("SLACK_SIGNING_SECRET")
		}
		resetSlackSecretsCache()
	})
}

// signedSlackRequest builds a request Slack would actually send.
func signedSlackRequest(secret, body string, ts time.Time) events.APIGatewayProxyRequest {
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + tsStr + ":" + body))
	return events.APIGatewayProxyRequest{
		Body: body,
		Headers: map[string]string{
			"X-Slack-Request-Timestamp": tsStr,
			"X-Slack-Signature":         "v0=" + hex.EncodeToString(mac.Sum(nil)),
		},
	}
}

// A genuinely signed request verifies.
func TestVerifySlackRequestAcceptsAGenuineSignature(t *testing.T) {
	withSigningSecret(t, testSigningSecret)

	now := time.Now()
	if err := verifySlackRequest(signedSlackRequest(testSigningSecret, `{"type":"tokens_revoked"}`, now), now); err != nil {
		t.Fatalf("a correctly signed request was rejected: %v", err)
	}
}

// Every way a request can fail verification must be rejected.
//
// Table-driven because the property is one thing — "not from Slack, not accepted"
// — and the failure that matters is any single row slipping through.
func TestVerifySlackRequestRejectsEverythingElse(t *testing.T) {
	withSigningSecret(t, testSigningSecret)
	now := time.Now()
	body := `{"type":"tokens_revoked"}`

	cases := map[string]events.APIGatewayProxyRequest{
		"no headers at all — the unauthenticated POST that was possible": {
			Body: body,
		},
		"signature present, timestamp missing": {
			Body:    body,
			Headers: map[string]string{"X-Slack-Signature": "v0=deadbeef"},
		},
		"timestamp present, signature missing": {
			Body:    body,
			Headers: map[string]string{"X-Slack-Request-Timestamp": strconv.FormatInt(now.Unix(), 10)},
		},
		"signature over a different body": func() events.APIGatewayProxyRequest {
			r := signedSlackRequest(testSigningSecret, `{"type":"tokens_revoked"}`, now)
			r.Body = `{"type":"tokens_revoked","token":"xoxb-attacker"}`
			return r
		}(),
		"signature from a different secret": signedSlackRequest("not-the-secret", body, now),
		"replay from six minutes ago": signedSlackRequest(testSigningSecret, body,
			now.Add(-6*time.Minute)),
		"dated six minutes into the future": signedSlackRequest(testSigningSecret, body,
			now.Add(6*time.Minute)),
		"malformed timestamp": func() events.APIGatewayProxyRequest {
			r := signedSlackRequest(testSigningSecret, body, now)
			r.Headers["X-Slack-Request-Timestamp"] = "not-a-number"
			return r
		}(),
		"truncated signature": func() events.APIGatewayProxyRequest {
			r := signedSlackRequest(testSigningSecret, body, now)
			r.Headers["X-Slack-Signature"] = r.Headers["X-Slack-Signature"][:20]
			return r
		}(),
		"wrong scheme version": func() events.APIGatewayProxyRequest {
			r := signedSlackRequest(testSigningSecret, body, now)
			r.Headers["X-Slack-Signature"] = strings.Replace(
				r.Headers["X-Slack-Signature"], "v0=", "v1=", 1)
			return r
		}(),
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if err := verifySlackRequest(req, now); err == nil {
				t.Error("request was accepted; it is not from Slack")
			}
		})
	}
}

// A base64-encoded body must verify against the DECODED bytes.
//
// API Gateway may base64 the body. Verifying the encoded form would fail every
// genuine request, and the tempting fix for that is to stop verifying.
func TestVerifySlackRequestHandlesABase64EncodedBody(t *testing.T) {
	withSigningSecret(t, testSigningSecret)
	now := time.Now()
	body := `{"type":"tokens_revoked"}`

	req := signedSlackRequest(testSigningSecret, body, now)
	req.Body = base64.StdEncoding.EncodeToString([]byte(body))
	req.IsBase64Encoded = true

	if err := verifySlackRequest(req, now); err != nil {
		t.Fatalf("a signed request with a base64 body was rejected: %v", err)
	}
}

// Header casing must not matter.
func TestVerifySlackRequestIsCaseInsensitiveAboutHeaders(t *testing.T) {
	withSigningSecret(t, testSigningSecret)
	now := time.Now()

	req := signedSlackRequest(testSigningSecret, `{}`, now)
	lower := map[string]string{}
	for k, v := range req.Headers {
		lower[strings.ToLower(k)] = v
	}
	req.Headers = lower

	if err := verifySlackRequest(req, now); err != nil {
		t.Fatalf("lowercase headers were rejected: %v", err)
	}
}

// A missing secret must fail closed, and be distinguishable from a bad signature.
//
// The two need opposite handling — one is an operator error worth a 500 and a log
// line, the other is a 401 — and collapsing them is how "we cannot verify" turns
// into "we do not verify".
func TestVerifySlackRequestFailsClosedWithNoSecret(t *testing.T) {
	withSigningSecret(t, "")

	err := verifySlackRequest(signedSlackRequest("anything", `{}`, time.Now()), time.Now())
	if err == nil {
		t.Fatal("verification passed with no signing secret configured — it must fail closed")
	}
	if !errors.Is(err, errSlackNotConfigured) {
		t.Errorf("a missing secret is not reported as errSlackNotConfigured, so the handler cannot "+
			"tell it apart from a forged signature: %v", err)
	}
}

// ── The rotate route must verify, and must not echo the token ────────────────

// The handler must call verifySlackRequest, and must not return the new token.
//
// Source-level because driving the handler needs Slack and DynamoDB; the point
// here is the wiring, which is what was missing. Anchored on the call and the
// response literal rather than on bare identifiers (CLAUDE.md rule 3).
func TestSlackRotateRouteVerifiesAndDoesNotEchoTheToken(t *testing.T) {
	src := readDashboardSource(t, "slack_oauth.go")

	fn := funcBody(t, src, "func handleSlackTokenRotate(")
	if !strings.Contains(fn, "verifySlackRequest(request") {
		t.Error("handleSlackTokenRotate does not call verifySlackRequest(request, ...). The route is " +
			"above getUserFromRequest and the API Gateway route is $default with " +
			"AuthorizationType: NONE, so without this it is reachable unauthenticated from the " +
			"public internet")
	}

	// The pre-fix response embedded the freshly minted token.
	if regexp.MustCompile(`"ok":true,"token":`).MatchString(fn) {
		t.Error("the rotate response still returns the new bot token in its body; a live credential " +
			"in an HTTP response lands in access logs and any proxy in between")
	}
	// And the error must not say whether a token matched a workspace.
	if strings.Contains(fn, `"Token rotation failed: %v"`) {
		t.Error("the error response still interpolates the failure reason, which tells a caller " +
			"whether a token is live")
	}
}

// Verification must come before any credential is touched.
//
// Ordering, not presence: verifying after the rotation would be a check that
// cannot prevent anything.
func TestSlackRotateVerifiesBeforeRotating(t *testing.T) {
	fn := funcBody(t, readDashboardSource(t, "slack_oauth.go"), "func handleSlackTokenRotate(")

	verify := strings.Index(fn, "verifySlackRequest(request")
	rotate := strings.Index(fn, "rotateTokenByCurrentToken(")
	if verify < 0 || rotate < 0 {
		t.Fatalf("could not locate both calls (verify=%d rotate=%d); this gate is anchored on "+
			"strings that no longer exist", verify, rotate)
	}
	if verify > rotate {
		t.Error("the request is rotated before it is verified")
	}
}

// ── Ownership checks must fail closed ────────────────────────────────────────

// No ownership check may be conditional on the attribute being present.
//
// The defect: `if v, ok := result.Item["user_id"]; ok { ... }` skipped the
// comparison entirely when the attribute was absent, or present but not a string.
// Discovery over the package rather than a named line, so the same shape cannot
// reappear in another handler.
func TestNoOwnershipCheckIsConditionalOnTheAttributeExisting(t *testing.T) {
	dir := dashboardDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	// `if <anything>, ok := <...>["user_id"]` — reading the owner out of the raw
	// item map with a presence test is the shape that fails open.
	conditional := regexp.MustCompile(`if\s+\w+\s*,\s*ok\s*:=\s*[^;\n]*\["user_id"\]`)

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body := readDashboardSource(t, e.Name())
		checked++
		if loc := conditional.FindString(body); loc != "" {
			t.Errorf("%s gates an ownership check on the attribute existing:\n\t%s\n"+
				"An absent or non-string user_id then skips the check and the record is returned "+
				"to any authenticated caller. Compare the unmarshalled field so a missing owner "+
				"is a mismatch, not a bypass (#821).", e.Name(), loc)
		}
	}
	if checked == 0 {
		t.Fatal("inspected no source files — discovery has broken, it has not proven the package clean")
	}
}

// handleGetWatch must compare the owner directly.
func TestGetWatchComparesTheOwnerDirectly(t *testing.T) {
	fn := funcBody(t, readDashboardSource(t, "watches.go"), "func handleGetWatch(")
	if !strings.Contains(fn, "w.UserID != userARN") {
		t.Error("handleGetWatch does not compare w.UserID against the caller. Every sibling handler " +
			"uses that idiom (sweeps.go, instances.go); this one read the raw item map and was the " +
			"only fail-open check in the file set")
	}
	if !strings.Contains(fn, "403") {
		t.Error("handleGetWatch does not return 403 on an ownership mismatch")
	}
}

// The owner field must not be serialised to the client.
//
// It is an authorization input. Returning it leaks the owning principal's IAM ARN
// to anyone allowed to read the record.
func TestWatchOwnerIsNotSerialisedToTheClient(t *testing.T) {
	src := readDashboardSource(t, "watches.go")
	if !regexp.MustCompile(`UserID\s+string\s+` + "`" + `json:"-"`).MatchString(src) {
		t.Error(`WatchInfo.UserID is not tagged json:"-"; the owning principal's IAM ARN would be ` +
			`returned in the watch payload`)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func dashboardDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

func readDashboardSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dashboardDir(t), name)) //nolint:gosec // fixed package dir
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// funcBody returns the text of the function whose declaration starts with decl,
// up to the next top-level func. Fails loudly rather than returning "" so a
// renamed function cannot make a gate silently vacuous.
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("could not find %q — this gate is reading a function that no longer exists "+
			"under that name", decl)
	}
	rest := src[i+len(decl):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}
