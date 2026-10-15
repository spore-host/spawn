package config

import (
	"strings"
	"testing"
)

// The trust principal for SpawnBotCrossAccount roles is spore-bot's own
// execution role in every environment, not a stack-generated name (#1046).
func TestBotLambdaRoleARN_EveryEnvironmentDefaultsToTheSporeBotRole(t *testing.T) {
	t.Setenv("SPORE_BOT_LAMBDA_ROLE_ARN", "")
	for _, env := range []string{"", "prod", "integ"} {
		t.Run("SPORE_ENV="+env, func(t *testing.T) {
			t.Setenv("SPORE_ENV", env)
			if got := GetBotLambdaRoleARN(); got != DefaultBotLambdaRoleARN {
				t.Errorf("GetBotLambdaRoleARN() = %q, want %q", got, DefaultBotLambdaRoleARN)
			}
		})
	}
	for env, arn := range botLambdaRoleARNs {
		if arn != DefaultBotLambdaRoleARN {
			t.Errorf("botLambdaRoleARNs[%s] = %q, want DefaultBotLambdaRoleARN %q", env, arn, DefaultBotLambdaRoleARN)
		}
	}
	if !strings.HasSuffix(DefaultBotLambdaRoleARN, ":role/"+SporeBotRoleName) {
		t.Errorf("DefaultBotLambdaRoleARN %q does not name role %q", DefaultBotLambdaRoleARN, SporeBotRoleName)
	}
}

func TestBotLambdaRoleARN_EnvOverrideWins(t *testing.T) {
	const override = "arn:aws:iam::111122223333:role/my-self-hosted-principal"
	t.Setenv("SPORE_BOT_LAMBDA_ROLE_ARN", override)
	if got := GetBotLambdaRoleARN(); got != override {
		t.Errorf("GetBotLambdaRoleARN() = %q, want override %q", got, override)
	}
}
