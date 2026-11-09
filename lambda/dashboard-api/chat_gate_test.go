package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
)

// The Slack endpoints are off unless the function's environment opts in
// (chat_gate.go, spore-host/spore-host#611).

// slackRoutes are the routes handler serves under /api/slack/, as a client
// calls them.
var slackRoutes = []struct{ method, path string }{
	{"GET", "/api/slack/oauth"},
	{"GET", "/api/slack/oauth/callback"},
	{"POST", "/api/slack/token/rotate"},
}

// TestSlackRoutesAreAllGated: every /api/slack/ route main.go routes is in
// slackRoutes, so a new one cannot skip this file's tests.
func TestSlackRoutesAreAllGated(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, r := range slackRoutes {
		known[r.path] = true
	}
	n := 0
	for _, line := range strings.Split(string(src), "\n") {
		i := strings.Index(line, `path == "/api/slack`)
		if i < 0 {
			continue
		}
		rest := line[i+len(`path == "`):]
		p := rest[:strings.Index(rest, `"`)]
		n++
		if !known[p] {
			t.Errorf("main.go routes %s, which slackRoutes does not list", p)
		}
	}
	if n == 0 {
		t.Fatal("found no /api/slack routes in main.go; the discovery is broken")
	}
}

func setChatEnv(t *testing.T, v string) {
	t.Helper()
	t.Setenv(experimentalChatEnv, v)
	if v == "" {
		_ = os.Unsetenv(experimentalChatEnv)
	}
}

// TestSlackEndpointsAre404WithoutTheOptIn: with the variable unset, or set to
// anything but a true value, each Slack route answers 404 with the reason and
// never loads an AWS config.
func TestSlackEndpointsAre404WithoutTheOptIn(t *testing.T) {
	loads := 0
	orig := loadAWSConfig
	t.Cleanup(func() { loadAWSConfig = orig })
	loadAWSConfig = func(context.Context) (aws.Config, error) {
		loads++
		return aws.Config{}, nil
	}
	for _, v := range []string{"", "0", "false", "please"} {
		setChatEnv(t, v)
		for _, r := range slackRoutes {
			resp, err := handler(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: r.method, Path: r.path})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 404 {
				t.Errorf("%s=%q %s %s: status %d, want 404", experimentalChatEnv, v, r.method, r.path, resp.StatusCode)
			}
			var body APIResponse
			if err := json.Unmarshal([]byte(resp.Body), &body); err != nil || body.Success || !strings.Contains(body.Error, "experimental") {
				t.Errorf("%s %s: body %q does not say why (%v)", r.method, r.path, resp.Body, err)
			}
		}
	}
	if loads != 0 {
		t.Errorf("a refused Slack request loaded an AWS config %d time(s)", loads)
	}
}

// TestSlackEndpointsServeWithTheOptIn: with SPAWN_EXPERIMENTAL_CHAT=1 the
// routes reach their handlers again. With no Slack client configured, the
// OAuth redirect answers its own 500, not the gate's 404.
func TestSlackEndpointsServeWithTheOptIn(t *testing.T) {
	setChatEnv(t, "1")
	t.Setenv("SLACK_CLIENT_ID", "")
	resp, err := handler(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/api/slack/oauth"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == 404 || !strings.Contains(resp.Body, "Slack OAuth not configured") {
		t.Errorf("opted in: status %d body %q, want the OAuth handler's own answer", resp.StatusCode, resp.Body)
	}

	t.Setenv("SLACK_CLIENT_ID", "fixture-client-id")
	resp, err = handler(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/api/slack/oauth"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 302 {
		t.Errorf("opted in with a client id: status %d, want the 302 to Slack", resp.StatusCode)
	}
}

// TestNonSlackRoutesAreNotGated: the gate covers /api/slack/ only.
func TestNonSlackRoutesAreNotGated(t *testing.T) {
	setChatEnv(t, "")
	resp, err := handler(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/api/strata/catalog"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("GET /api/strata/catalog: status %d, want 200 — the chat gate must not cover it", resp.StatusCode)
	}
	if _, handled := chatEndpointGate("/api/slackish"); handled {
		t.Error("the gate matched /api/slackish, which is not under /api/slack/")
	}
}
