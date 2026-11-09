package config

import (
	"strings"
	"testing"
)

// spore.host no longer hosts a chat bot (spore-host/spore-host#611): the
// notify URL and the bot principal have no built-in default, and a value
// naming the retired hosted bot is refused rather than used.

// Assembled, so this file is not a match for the repo-wide scan in
// bot_principal_test.go.
var (
	retiredHostedRole = "arn:aws:iam::" + retiredHostedBotAccount + ":role/" + "spore-bot-role"
	retiredPrismRole  = "arn:aws:iam::" + retiredHostedBotAccount + ":role/" + "prism-bot-PrismBotFunctionRole-U2vZFZXgWBeM"
)

func TestBotEndpointsHaveNoDefault(t *testing.T) {
	t.Setenv(EnvNotifyURL, "")
	t.Setenv(EnvBotLambdaRoleARN, "")
	for _, env := range []string{"", "prod", "integ"} {
		t.Run("SPORE_ENV="+env, func(t *testing.T) {
			t.Setenv("SPORE_ENV", env)
			if got := GetNotifyURL(); got != "" {
				t.Errorf("GetNotifyURL() = %q with nothing set, want \"\": there is no hosted bot to default to", got)
			}
			if got := GetBotLambdaRoleARN(); got != "" {
				t.Errorf("GetBotLambdaRoleARN() = %q with nothing set, want \"\"", got)
			}
			if _, err := ResolveNotifyURL(); err == nil || !strings.Contains(err.Error(), EnvNotifyURL+" is not set") {
				t.Errorf("ResolveNotifyURL() with nothing set: err = %v, want one naming %s", err, EnvNotifyURL)
			}
			if _, err := ResolveBotLambdaRoleARN(); err == nil || !strings.Contains(err.Error(), EnvBotLambdaRoleARN+" is not set") {
				t.Errorf("ResolveBotLambdaRoleARN() with nothing set: err = %v, want one naming %s", err, EnvBotLambdaRoleARN)
			}
		})
	}
}

func TestResolveNotifyURL(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"https://abcdefghijklmnopqrstuvwxyz012345.lambda-url.eu-west-1.on.aws", true},
		{"https://bot.example.org/", true},
		{"http://bot.example.org", false}, // spored posts its identity document
		{"bot.example.org", false},
		{"https://", false},
		// The retired hosted bot's Function URL and API.
		{"https://" + retiredHostedBotURLMarkers[0] + ".lambda-url.us-east-1.on.aws", false},
		{"https://" + strings.ToUpper(retiredHostedBotURLMarkers[0]) + ".lambda-url.us-east-1.on.aws", false},
		{"https://" + retiredHostedBotURLMarkers[1] + ".execute-api.us-east-1.amazonaws.com", false},
	} {
		t.Setenv(EnvNotifyURL, tc.value)
		got, err := ResolveNotifyURL()
		if (err == nil) != tc.ok {
			t.Errorf("%s=%q: err = %v, want ok=%v", EnvNotifyURL, tc.value, err, tc.ok)
		}
		if tc.ok && got != tc.value {
			t.Errorf("%s=%q: resolved %q", EnvNotifyURL, tc.value, got)
		}
	}
}

func TestResolveBotLambdaRoleARN(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"arn:aws:iam::111122223333:role/my-self-hosted-principal", true},
		{"arn:aws:iam::111122223333:role/service/spore-bot-SporeBotFunctionRole-ABC", true},
		// Another role in the infra account that is not a bot is not the retired one.
		{"arn:aws:iam::" + retiredHostedBotAccount + ":role/SomethingElse", true},
		{retiredHostedRole, false},
		{retiredPrismRole, false},
		{"spore-bot-role", false},
		{"arn:aws:iam::12345:role/x", false},
	} {
		t.Setenv(EnvBotLambdaRoleARN, tc.value)
		got, err := ResolveBotLambdaRoleARN()
		if (err == nil) != tc.ok {
			t.Errorf("%s=%q: err = %v, want ok=%v", EnvBotLambdaRoleARN, tc.value, err, tc.ok)
		}
		if tc.ok && got != tc.value {
			t.Errorf("%s=%q: resolved %q", EnvBotLambdaRoleARN, tc.value, got)
		}
	}
}
