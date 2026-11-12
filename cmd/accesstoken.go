package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/accesstoken"
	"github.com/spore-host/spawn/pkg/aws"
)

// Opening a DCV session or a web app (pkg/accesstoken). spored publishes only
// that the session is ready; this side mints a token, registers its SHA-256 on
// the instance over SSM, and builds the URL locally. The token is never in a
// tag, in user-data or in SSM's command history, and every connect gets a new
// one, valid for accesstoken.DefaultTTL.

// readyAuthTag mirrors pkg/agent: "token" means register a token, "none" that
// the web proxy is ungated. Absent means a spored that predates this, which
// published its token in legacyReadyTokenTag.
const (
	readyAuthTag        = "spawn:ready-auth"
	readyAuthToken      = "token"
	readyAuthNone       = "none"
	legacyReadyTokenTag = "spawn:ready-token"
)

// shellRunner runs a script on an instance over SSM (aws.Client.RunShellScript).
type shellRunner func(ctx context.Context, region, instanceID, script string) (*aws.SSMRunResult, error)

// clientShellRunner adapts a client to a shellRunner.
func clientShellRunner(client *aws.Client) shellRunner {
	return func(ctx context.Context, region, instanceID, script string) (*aws.SSMRunResult, error) {
		return client.RunShellScript(ctx, region, instanceID, script, 60*time.Second)
	}
}

// errAgentPredatesAccessTokens: the instance's spored has no `access-token`
// subcommand.
var errAgentPredatesAccessTokens = errors.New("the instance's spored predates access-token registration")

// registerAttempts / registerRetryDelay bound how long a just-started instance
// gets for its SSM agent to come online before registration gives up.
const (
	registerAttempts   = 6
	registerRetryDelay = 10 * time.Second
)

// issueAccessToken mints a token for kind and registers its hash on the
// instance, returning the token. Only the hash goes over SSM.
func issueAccessToken(ctx context.Context, run shellRunner, region, instanceID string, kind accesstoken.Kind, ttl time.Duration) (string, error) {
	return issueAccessTokenWith(ctx, run, region, instanceID, kind, ttl, registerRetryDelay)
}

func issueAccessTokenWith(ctx context.Context, run shellRunner, region, instanceID string, kind accesstoken.Kind, ttl, retry time.Duration) (string, error) {
	token, err := accesstoken.Mint()
	if err != nil {
		return "", err
	}
	script, err := accesstoken.AddCommand(kind, accesstoken.Hash(token), ttl)
	if err != nil {
		return "", err
	}
	var lastErr error
	for i := 0; i < registerAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(retry):
			}
		}
		res, err := run(ctx, region, instanceID, script)
		if err != nil {
			// SendCommand is refused until the SSM agent is online, which on a
			// just-started instance can trail the readiness tag.
			lastErr = err
			continue
		}
		if res != nil && res.Status == "Success" && res.ResponseCode == 0 {
			return token, nil
		}
		out := ""
		if res != nil {
			out = strings.TrimSpace(res.Stderr + " " + res.Stdout)
		}
		if strings.Contains(out, "unknown command") {
			return "", errAgentPredatesAccessTokens
		}
		return "", fmt.Errorf("register access token on %s: %s", instanceID, out)
	}
	return "", fmt.Errorf("register access token on %s over SSM: %w", instanceID, lastErr)
}

// sessionToken returns the token the browser presents for a session that
// scan reports ready: a freshly registered one, "" for an ungated web app, or
// — only for an instance whose spored predates registration — the token that
// spored published in a tag, with a warning to w. An error means the session
// cannot be opened with a token; the caller still prints the bare URL.
func sessionToken(ctx context.Context, run shellRunner, region, instanceID string, scan dcvScanResult, kind accesstoken.Kind, ttl time.Duration, w io.Writer) (string, error) {
	switch scan.auth {
	case readyAuthNone:
		return "", nil
	case readyAuthToken:
		return issueAccessToken(ctx, run, region, instanceID, kind, ttl)
	}
	if scan.legacyToken != "" {
		fmt.Fprintf(w, "⚠️  %s runs an older spored that publishes its session token in an EC2 tag, readable by anyone in the account with ec2:DescribeTags. Upgrade it (spawn upgrade-spored %s) or relaunch.\n", instanceID, instanceID)
		return scan.legacyToken, nil
	}
	return "", nil
}

// sessionURL is the browser URL for a session on host.
func sessionURL(kind accesstoken.Kind, host, token, dcvSessionID string) string {
	if kind == accesstoken.KindDCV {
		return accesstoken.DCVURL(host, token, dcvSessionID)
	}
	return accesstoken.WebURL(host, token)
}
