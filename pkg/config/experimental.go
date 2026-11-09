package config

import (
	"os"
	"strings"
)

// Chat-platform notifications (Slack, Teams, Discord, through spore-bot) are
// walled off for 1.0 (spore-host/spore-host#611, #644): too much setup for a
// supported feature, and the hosted spore-bot is being decommissioned. The code
// stays so the feature can return after 1.0; every user-facing entry point
// requires this explicit opt-in first.
//
// One name, used everywhere: the CLI, the dashboard-api Lambda's environment,
// the refusal message and the docs.
const (
	// ExperimentalChatEnv opts in to the chat integrations.
	ExperimentalChatEnv = "SPAWN_EXPERIMENTAL_CHAT"
	// ExperimentalChatConfigKey is the same opt-in in ~/.spawn/config.yaml.
	ExperimentalChatConfigKey = "experimental.chat"
)

// ExperimentalChatRefusal is what every gated entry point says without the
// opt-in.
const ExperimentalChatRefusal = "chat-platform notifications (Slack/Teams/Discord) are experimental and not supported in this release; set " +
	ExperimentalChatEnv + "=1 to use them"

// ExperimentalConfig is the `experimental:` section of ~/.spawn/config.yaml.
// Each field is a tri-state pointer, like DNSConfig.Enabled: an absent key is
// "not said", not "false".
type ExperimentalConfig struct {
	Chat *bool `yaml:"chat,omitempty"`
}

// ExperimentalChatEnabled reports whether the chat integrations are opted in,
// and which layer decided. Precedence follows the shared config (flag > env >
// file > default), less the flag, which this has none of:
//
//   - SPAWN_EXPERIMENTAL_CHAT, when set to a recognised boolean (1/true/yes/on
//     or 0/false/no/off), decides, so a 0 in the environment overrides a true
//     in the file;
//   - else experimental.chat in ~/.spawn/config.yaml;
//   - else off.
//
// An unrecognised environment value is off, rather than falling through to the
// file: a typo must not opt anyone in.
func ExperimentalChatEnabled() (enabled bool, source string) {
	if raw, ok := os.LookupEnv(ExperimentalChatEnv); ok && strings.TrimSpace(raw) != "" {
		v, known := parseOptInBool(raw)
		if !known {
			return false, ExperimentalChatEnv + " (unrecognised value)"
		}
		return v, ExperimentalChatEnv
	}
	if cfg, err := loadFromFile(); err == nil && cfg != nil && cfg.Experimental.Chat != nil {
		return *cfg.Experimental.Chat, ExperimentalChatConfigKey
	}
	return false, ""
}

// ChatEnabled is ExperimentalChatEnabled without the source.
func ChatEnabled() bool {
	on, _ := ExperimentalChatEnabled()
	return on
}

func parseOptInBool(raw string) (value, known bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}
