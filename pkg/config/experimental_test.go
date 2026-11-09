package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// chatOptInEnv points HOME at a temp dir holding fileBody as
// ~/.spawn/config.yaml ("" for no file) and sets SPAWN_EXPERIMENTAL_CHAT to env
// (unset when env is nil).
func chatOptInEnv(t *testing.T, env *string, fileBody string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ExperimentalChatEnv, "")
	if env == nil {
		_ = os.Unsetenv(ExperimentalChatEnv)
	} else {
		t.Setenv(ExperimentalChatEnv, *env)
	}
	if fileBody != "" {
		if err := os.MkdirAll(filepath.Join(home, ".spawn"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, configFileName), []byte(fileBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestExperimentalChatPrecedence: env > file > default (off), as the shared
// config resolves; an explicit 0 in the environment closes a file opt-in, and
// an unrecognised value is off, never an opt-in.
func TestExperimentalChatPrecedence(t *testing.T) {
	s := func(v string) *string { return &v }
	const fileOn, fileOff, fileOther = "experimental:\n  chat: true\n", "experimental:\n  chat: false\n", "defaults:\n  idle_timeout: 1h\n"
	cases := []struct {
		name       string
		env        *string
		file       string
		want       bool
		wantSource string
	}{
		{"nothing set", nil, "", false, ""},
		{"a config file without the key", nil, fileOther, false, ""},
		{"env 1", s("1"), "", true, ExperimentalChatEnv},
		{"env true", s("true"), "", true, ExperimentalChatEnv},
		{"env ON", s("ON"), "", true, ExperimentalChatEnv},
		{"file true", nil, fileOn, true, ExperimentalChatConfigKey},
		{"file false", nil, fileOff, false, ExperimentalChatConfigKey},
		{"env 0 over file true", s("0"), fileOn, false, ExperimentalChatEnv},
		{"env 1 over file false", s("1"), fileOff, true, ExperimentalChatEnv},
		{"env empty falls through to the file", s(""), fileOn, true, ExperimentalChatConfigKey},
		{"env typo is off, even over file true", s("ture"), fileOn, false, ExperimentalChatEnv + " (unrecognised value)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatOptInEnv(t, tc.env, tc.file)
			got, src := ExperimentalChatEnabled()
			if got != tc.want || src != tc.wantSource {
				t.Errorf("ExperimentalChatEnabled() = %v, %q; want %v, %q", got, src, tc.want, tc.wantSource)
			}
			if ChatEnabled() != tc.want {
				t.Errorf("ChatEnabled() = %v, want %v", ChatEnabled(), tc.want)
			}
		})
	}
}

// TestSettingADefaultKeepsTheChatOptIn: `spawn defaults set` rewrites
// ~/.spawn/config.yaml from the Config struct, so the experimental section
// must be part of it or the first `defaults set` would silently drop an
// opt-in.
func TestSettingADefaultKeepsTheChatOptIn(t *testing.T) {
	chatOptInEnv(t, nil, "experimental:\n  chat: true\n")
	if err := SetLaunchDefault("idle-timeout", "1h"); err != nil {
		t.Fatal(err)
	}
	if !ChatEnabled() {
		t.Error("defaults set dropped experimental.chat from ~/.spawn/config.yaml")
	}
}

// TestDashboardAPIUsesTheSameOptIn: the dashboard-api Lambda gates its Slack
// endpoints on its own copy of the variable name (it does not import this
// package), which must be this one.
func TestDashboardAPIUsesTheSameOptIn(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "lambda", "dashboard-api", "chat_gate.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^const experimentalChatEnv = "([^"]*)"$`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatal("lambda/dashboard-api/chat_gate.go declares no `const experimentalChatEnv = \"...\"`")
	}
	if string(m[1]) != ExperimentalChatEnv {
		t.Errorf("dashboard-api gates on %q, the CLI on %q: one name, used everywhere", m[1], ExperimentalChatEnv)
	}
}
