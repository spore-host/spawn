package main

import (
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// The Slack endpoints (/api/slack/oauth, /api/slack/oauth/callback,
// /api/slack/token/rotate) are walled off for 1.0 with the rest of the
// chat-platform integrations (spore-host/spore-host#611, #644). They are kept so
// the feature can return after 1.0, and served only when this function's
// environment opts in with the same variable the CLI reads.
//
// Off by default: the hosted spawn-dashboard-api sets no such variable (its
// deploy, lambda/dashboard-api/deploy.sh and the CI deploy, update the code and
// leave the environment alone), so its next deploy turns the endpoints off.
// A self-hosted deployment that still wants them sets
// SPAWN_EXPERIMENTAL_CHAT=1 on the function.
//
// deploychecklist:note Slack endpoints (/api/slack/*) answer 404 unless the function's environment sets SPAWN_EXPERIMENTAL_CHAT=1; the hosted spawn-dashboard-api must NOT set it for 1.0 (spore-host/spore-host#611). Check with `aws lambda get-function-configuration --function-name spawn-dashboard-api --query Environment.Variables.SPAWN_EXPERIMENTAL_CHAT`.

// experimentalChatEnv must equal pkg/config.ExperimentalChatEnv, which this
// module does not import (it would link pkg/config's dependencies into the
// Lambda for one string). pkg/config's TestDashboardAPIUsesTheSameOptIn holds
// the two together.
const experimentalChatEnv = "SPAWN_EXPERIMENTAL_CHAT"

// slackPathPrefix is every path the gate covers.
const slackPathPrefix = "/api/slack/"

// chatEndpointsDisabledMessage is the body of a refused Slack request.
const chatEndpointsDisabledMessage = "chat-platform integrations (Slack/Teams/Discord) are experimental and not enabled on this deployment; set " +
	experimentalChatEnv + "=1 on the function to serve them"

// chatEndpointsEnabled reports whether this function serves the Slack
// endpoints. Only an explicit true value opts in.
func chatEndpointsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(experimentalChatEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// chatEndpointGate answers a Slack-endpoint request when the endpoints are off.
// handled is false for every other path, and for every path when they are on.
// It is consulted before the AWS config is loaded, so a refused request makes
// no AWS call.
func chatEndpointGate(path string) (resp events.APIGatewayProxyResponse, handled bool) {
	if !strings.HasPrefix(path, slackPathPrefix) && path != strings.TrimSuffix(slackPathPrefix, "/") {
		return resp, false
	}
	if chatEndpointsEnabled() {
		return resp, false
	}
	return errorResponse(404, chatEndpointsDisabledMessage), true
}
