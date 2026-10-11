package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// Slack request verification (#821, GHSA-qc2h-6qqf-wcm4).
//
// POST /api/slack/token/rotate was routed ABOVE getUserFromRequest, alongside the
// two OAuth routes — but unlike those, with no comment saying why, and the API
// Gateway route is `$default` with AuthorizationType: NONE. So it was reachable
// unauthenticated from the public internet, and it took the workspace's current
// bot token out of the request body, rotated it with Slack, and returned the NEW
// token in the response:
//
//	{"ok":true,"token":"xoxb-..."}
//
// Possession of a (possibly leaked, possibly already-stale) token was therefore
// the only credential needed to mint a fresh valid one — while the legitimate
// stored token was replaced, so the real owner lost theirs. Verified against
// production with a deliberately invalid token: the endpoint answered
// `500 {"error":"Token rotation failed: workspace not found for token"}`, which
// shows the handler ran with no gate in front of it. Nothing was written, because
// the lookup fails before the update.
//
// The fix is the mechanism Slack provides and this repo already had the secret
// for: every inbound Slack request carries an HMAC signature over the raw body,
// and `slackSigningSecret()` has existed all along. It was stored in Secrets
// Manager and on every workspace row, and never used to verify anything.

// slackSignatureMaxSkew bounds replay. Slack's own guidance is five minutes.
const slackSignatureMaxSkew = 5 * time.Minute

// slackSignatureVersion is the only scheme Slack has defined.
const slackSignatureVersion = "v0"

// errSlackNotConfigured distinguishes "we cannot verify" from "verification
// failed", because the two need opposite handling: a missing secret is an
// operator error that must fail closed loudly, not a 401 for the caller.
var errSlackNotConfigured = errors.New("slack signing secret is not configured")

// rawSlackBody returns the body exactly as Slack signed it.
//
// API Gateway may hand the Lambda a base64-encoded body. Signing the encoded form
// would fail every genuine request, and — worse for a security control — the
// natural "fix" for that is to stop verifying.
func rawSlackBody(request events.APIGatewayProxyRequest) ([]byte, error) {
	if !request.IsBase64Encoded {
		return []byte(request.Body), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(request.Body)
	if err != nil {
		return nil, fmt.Errorf("decode base64 body: %w", err)
	}
	return decoded, nil
}

// headerCaseInsensitive reads a header regardless of the casing the gateway used.
// API Gateway v1 preserves the sender's casing; Slack sends X-Slack-*, but relying
// on that is how a verification step quietly stops matching.
func headerCaseInsensitive(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	lower := ""
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower += string(c)
	}
	for k, v := range headers {
		kl := ""
		for i := 0; i < len(k); i++ {
			c := k[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			kl += string(c)
		}
		if kl == lower {
			return v
		}
	}
	return ""
}

// verifySlackRequest checks the HMAC signature and timestamp on an inbound Slack
// request. It returns nil only when the request genuinely came from the Slack app
// this deployment is configured for.
func verifySlackRequest(request events.APIGatewayProxyRequest, now time.Time) error {
	secret := slackSigningSecret()
	if secret == "" {
		return errSlackNotConfigured
	}

	sig := headerCaseInsensitive(request.Headers, "X-Slack-Signature")
	tsRaw := headerCaseInsensitive(request.Headers, "X-Slack-Request-Timestamp")
	if sig == "" || tsRaw == "" {
		return errors.New("missing Slack signature headers")
	}

	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return fmt.Errorf("malformed X-Slack-Request-Timestamp: %w", err)
	}
	// Absolute skew, so a far-FUTURE timestamp is rejected too. Checking only
	// "now - ts > max" would let a replay be refreshed indefinitely by dating it
	// forward.
	skew := now.Unix() - ts
	if skew < 0 {
		skew = -skew
	}
	if time.Duration(skew)*time.Second > slackSignatureMaxSkew {
		return fmt.Errorf("Slack timestamp is %ds away from now, outside the %s replay window",
			skew, slackSignatureMaxSkew)
	}

	body, err := rawSlackBody(request)
	if err != nil {
		return err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	// basestring is v0:{timestamp}:{raw body}
	mac.Write([]byte(slackSignatureVersion + ":" + tsRaw + ":"))
	mac.Write(body)
	want := slackSignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))

	// Constant-time: a byte-by-byte comparison of an HMAC is a timing oracle for
	// forging one.
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("Slack signature does not match")
	}
	return nil
}
