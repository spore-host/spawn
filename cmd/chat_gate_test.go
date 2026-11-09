package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/launchconfig"
)

// The chat-platform wall (chat_gate.go, spore-host/spore-host#611): every chat
// entry point refuses without SPAWN_EXPERIMENTAL_CHAT, before any AWS call, and
// works with it; help hides them; and a default launch puts nothing
// chat-related on the instance.

// A self-hosted bot's endpoints, for tests that opt in.
const (
	selfHostedNotifyURL = "https://bot.example.invalid"
	selfHostedBotRole   = "arn:aws:iam::111122223333:role/my-spore-bot-role"
)

// chatTestEnv isolates the opt-in's two sources and AWS. HOME is a temp dir
// (no ~/.spawn/config.yaml unless the test writes one), the variable is unset
// unless optIn, and every AWS request, through the loadAWSConfig seam or any
// SDK client built from the environment, is counted and refused. The returned
// func reports the config loads and requests seen.
func chatTestEnv(t *testing.T, optIn bool) (home string, awsActivity func() (loads, requests int64)) {
	t.Helper()
	isolateAWSEnv(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	if optIn {
		t.Setenv(spawnconfig.ExperimentalChatEnv, "1")
		// Opted in, chat also needs the operator's own bot: spore.host hosts
		// none (#611). TestChatOptInNeedsTheOperatorsOwnBot unsets these.
		t.Setenv(spawnconfig.EnvNotifyURL, selfHostedNotifyURL)
		t.Setenv(spawnconfig.EnvBotLambdaRoleARN, selfHostedBotRole)
	} else {
		t.Setenv(spawnconfig.ExperimentalChatEnv, "")
		_ = os.Unsetenv(spawnconfig.ExperimentalChatEnv)
	}

	var loads, requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDCHATGATETEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-a-secret")

	real := loadAWSConfig
	loadAWSConfig = func(ctx context.Context, region string) (sdkaws.Config, error) {
		loads.Add(1)
		cfg, err := real(ctx, region)
		cfg.Retryer = func() sdkaws.Retryer { return sdkaws.NopRetryer{} }
		return cfg, err
	}
	t.Cleanup(func() { loadAWSConfig = real })
	return home, func() (int64, int64) { return loads.Load(), requests.Load() }
}

// resetCommandFlags puts a command tree's local flags back to their defaults,
// so one test's arguments do not leak into the next run of rootCmd.
func resetCommandFlags(t *testing.T, root *cobra.Command) {
	t.Helper()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		// Local only: the inherited root flags (--output, --region) are the
		// root's to reset.
		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(nil)
			} else {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	t.Cleanup(func() { walk(root) })
}

// notifyInvocations discovers every runnable command under `spawn notify`,
// including the hidden deprecated shims, as a command line that would
// otherwise pass its own required-flag checks and reach AWS.
func notifyInvocations(t *testing.T) [][]string {
	t.Helper()
	// Enough for each command's own validation, so that with the opt-in the
	// command gets as far as AWS.
	fill := map[string]string{
		"platform": "slack", "workspace-id": "T1", "user-id": "U1", "nickname": "n",
		"instance": "i-0123456789abcdef0", "signing-secret": "not-a-secret",
	}
	var out [][]string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() {
			args := strings.Fields(strings.TrimPrefix(c.CommandPath(), "spawn "))
			c.Flags().VisitAll(func(f *pflag.Flag) {
				if v, ok := fill[f.Name]; ok {
					args = append(args, "--"+f.Name, v)
				}
				if f.Name == "yes" {
					args = append(args, "--yes")
				}
			})
			out = append(out, args)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(botCmd)
	// notify itself, register/deregister/list/enable/disable, workspace
	// add/remove/list/destroy and four deprecated shims.
	if len(out) < 14 {
		t.Fatalf("discovered only %d runnable notify commands; the discovery is broken: %v", len(out), out)
	}
	return out
}

// chatLaunchCases are launches that set a chat flag, as flag name/value pairs
// in order. They run through runLaunch directly rather than rootCmd, which
// would add a --help flag to launch's own flag set and break the gates that
// discover launch's flags.
var chatLaunchCases = [][][2]string{
	{{"instance-type", "t4g.small"}, {"slack-workspace", "T1"}},
	{{"instance-type", "t4g.small"}, {"notify-platform", "teams"}},
	// An explicitly empty value is still the flag.
	{{"instance-type", "t4g.small"}, {"slack-workspace", ""}},
	// The sweep path dispatches early; the gate is ahead of it.
	{{"instance-type", "t4g.small"}, {"params", `[{"a":1}]`}, {"slack-workspace", "T1"}},
}

// assertChatRefusal checks err is the refusal, that it exits non-zero, and
// that nothing reached AWS or the config file.
func assertChatRefusal(t *testing.T, err error, home string, activity func() (int64, int64)) {
	t.Helper()
	if !errors.Is(err, errChatExperimental) {
		t.Fatalf("err = %v, want the experimental-chat refusal", err)
	}
	if !strings.Contains(err.Error(), "set SPAWN_EXPERIMENTAL_CHAT=1 to use them") {
		t.Errorf("the refusal does not say how to opt in: %v", err)
	}
	if code := exitForError(err); code == 0 {
		t.Errorf("exit code %d, want non-zero", code)
	}
	if loads, reqs := activity(); loads != 0 || reqs != 0 {
		t.Errorf("refused, but loaded an AWS config %d time(s) and sent %d request(s)", loads, reqs)
	}
	if _, err := os.Stat(filepath.Join(home, ".spawn", "config.yaml")); err == nil {
		t.Error("refused, but wrote ~/.spawn/config.yaml")
	}
}

func TestChatEntryPointsRefuseWithoutTheOptIn(t *testing.T) {
	resetRootOutputFlag(t)
	resetCommandFlags(t, botCmd)
	resetCommandFlags(t, defaultsCmd)
	invocations := append(notifyInvocations(t),
		[]string{"defaults", "set", "slack-workspace", "T1"},
		// The `bot` alias reaches the same gate.
		[]string{"bot", "list", "--platform", "slack", "--workspace-id", "T1"},
	)
	for _, args := range invocations {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home, activity := chatTestEnv(t, false)
			assertChatRefusal(t, runRootForTest(t, args...), home, activity)
		})
	}
	for _, flags := range chatLaunchCases {
		var name []string
		for _, f := range flags {
			name = append(name, "--"+f[0]+"="+f[1])
		}
		t.Run("launch "+strings.Join(name, " "), func(t *testing.T) {
			keepLaunchGlobals(t)
			home, activity := chatTestEnv(t, false)
			for _, f := range flags {
				setLaunchFlag(t, f[0], f[1])
			}
			assertChatRefusal(t, runLaunch(launchCmd, []string{"box"}), home, activity)
		})
	}
}

func TestChatEntryPointsPassTheGateWithTheOptIn(t *testing.T) {
	resetRootOutputFlag(t)
	keepLaunchGlobals(t)
	resetCommandFlags(t, botCmd)
	resetCommandFlags(t, defaultsCmd)
	for _, args := range notifyInvocations(t) {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, activity := chatTestEnv(t, true)
			err := runRootForTest(t, args...)
			if errors.Is(err, errChatExperimental) {
				t.Fatalf("opted in, and still refused: %v", err)
			}
			// `spawn notify` alone prints its help; every other command gets
			// as far as AWS, which the test refuses.
			if len(args) > 1 {
				if loads, _ := activity(); loads == 0 {
					t.Errorf("opted in, but never loaded an AWS config (err: %v): the command did not run", err)
				}
			}
		})
	}

	t.Run("defaults set slack-workspace", func(t *testing.T) {
		home, _ := chatTestEnv(t, true)
		if err := runRootForTest(t, "defaults", "set", "slack-workspace", "T1"); err != nil {
			t.Fatalf("opted in: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(home, ".spawn", "config.yaml"))
		if err != nil || !strings.Contains(string(b), "slack_workspace: T1") {
			t.Errorf("the default was not stored (%v):\n%s", err, b)
		}
	})

	t.Run("launch flags", func(t *testing.T) {
		chatTestEnv(t, true)
		for _, n := range chatLaunchFlags {
			setLaunchFlag(t, n, "x")
		}
		if err := chatLaunchFlagsGate(launchCmd); err != nil {
			t.Errorf("opted in, and the launch gate refused: %v", err)
		}
	})
}

// TestChatOptInFromTheConfigFile: experimental.chat in ~/.spawn/config.yaml
// opens the gate too, and SPAWN_EXPERIMENTAL_CHAT=0 closes it again.
func TestChatOptInFromTheConfigFile(t *testing.T) {
	resetCommandFlags(t, botCmd)
	home, _ := chatTestEnv(t, false)
	if err := os.MkdirAll(filepath.Join(home, ".spawn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".spawn", "config.yaml"), []byte("experimental:\n  chat: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRootForTest(t, "notify", "list", "--platform", "slack", "--workspace-id", "T1"); errors.Is(err, errChatExperimental) {
		t.Errorf("experimental.chat: true in the config file, and still refused")
	}
	t.Setenv(spawnconfig.ExperimentalChatEnv, "0")
	if err := runRootForTest(t, "notify", "list", "--platform", "slack", "--workspace-id", "T1"); !errors.Is(err, errChatExperimental) {
		t.Errorf("SPAWN_EXPERIMENTAL_CHAT=0 over a file opt-in: err = %v, want the refusal", err)
	}
}

// TestChatEntryPointsAreHiddenFromHelp: without the opt-in, neither the root
// help, nor launch's, nor defaults' mentions them; with it, all three do.
func TestChatEntryPointsAreHiddenFromHelp(t *testing.T) {
	t.Cleanup(func() { applyChatVisibility(false) })
	help := func(c *cobra.Command) string {
		var b bytes.Buffer
		c.SetOut(&b)
		defer c.SetOut(nil)
		if err := c.Help(); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	check := func(visible bool) {
		applyChatVisibility(visible)
		cases := map[string]struct {
			cmd  *cobra.Command
			text string
		}{
			"spawn --help lists notify":                      {rootCmd, "\n  notify "},
			"spawn launch --help lists --slack-workspace":    {launchCmd, "--slack-workspace"},
			"spawn launch --help lists --notify-platform":    {launchCmd, "--notify-platform"},
			"spawn defaults --help lists slack-workspace":    {defaultsCmd, "slack-workspace"},
			"spawn defaults --help has the chat set example": {defaultsCmd, "defaults set slack-workspace"},
		}
		for what, c := range cases {
			if got := strings.Contains(help(c.cmd), c.text); got != visible {
				t.Errorf("chat visible=%v: %s = %v", visible, what, got)
			}
		}
	}
	check(false)
	check(true)
	check(false)
}

// TestChatEntryPointsStartHidden: the state before Execute reads the opt-in is
// hidden, so nothing that renders help without Execute shows them.
func TestChatEntryPointsStartHidden(t *testing.T) {
	if !botCmd.Hidden {
		t.Error("spawn notify is not Hidden in its declaration")
	}
	for _, n := range chatLaunchFlags {
		if f := launchCmd.Flags().Lookup(n); f == nil || !f.Hidden {
			t.Errorf("--%s is not marked hidden where it is registered", n)
		}
	}
	if strings.Contains(defaultsLongHelp(false), "slack-workspace") {
		t.Error("defaults' default help lists slack-workspace")
	}
}

// chatSettingKeys are the launch config object's chat settings.
var chatSettingKeys = []string{launchconfig.NotifyURL, launchconfig.NotifyCommand, launchconfig.NotifyPlatform, launchconfig.SlackWorkspaceID}

// TestDefaultLaunchConfiguresNoChat: a launch without the opt-in, including
// one with a slack_workspace default stored in ~/.spawn/config.yaml, builds a
// config whose launch config object (what spored reads) carries no chat
// setting, and no chat tag; with the opt-in it carries them.
func TestDefaultLaunchConfiguresNoChat(t *testing.T) {
	keepLaunchGlobals(t)
	prevType, prevSlack, prevPlatform := instanceType, slackWorkspaceID, notifyPlatform
	t.Cleanup(func() { instanceType, slackWorkspaceID, notifyPlatform = prevType, prevSlack, prevPlatform })
	instanceType = "m7i.large"

	settingsOf := func(t *testing.T) (*aws.LaunchConfig, string) {
		t.Helper()
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		body, _, err := aws.LaunchConfigObject(*cfg)
		if err != nil {
			t.Fatalf("LaunchConfigObject: %v", err)
		}
		return cfg, string(body)
	}
	storeDefault := func(t *testing.T, home string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(home, ".spawn"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".spawn", "config.yaml"), []byte("defaults:\n  slack_workspace: T03NE3GTY\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("off, with a stored default", func(t *testing.T) {
		home, _ := chatTestEnv(t, false)
		storeDefault(t, home)
		slackWorkspaceID, notifyPlatform = "", ""
		applyLaunchDefaults(launchCmd)
		if slackWorkspaceID != "" {
			t.Fatalf("the stored slack_workspace default was applied without the opt-in: %q", slackWorkspaceID)
		}
		cfg, body := settingsOf(t)
		if cfg.NotifyURL != "" || cfg.SlackWorkspaceID != "" || cfg.NotifyPlatform != "" || cfg.NotifyCommand != "" {
			t.Errorf("config carries chat settings: url=%q workspace=%q platform=%q command=%q",
				cfg.NotifyURL, cfg.SlackWorkspaceID, cfg.NotifyPlatform, cfg.NotifyCommand)
		}
		for _, k := range chatSettingKeys {
			if strings.Contains(body, `"`+k+`"`) {
				t.Errorf("the launch config object carries %s:\n%s", k, body)
			}
		}
		for k := range cfg.Tags {
			if strings.Contains(k, "notify") || strings.Contains(k, "slack") {
				t.Errorf("tag %s is a chat setting", k)
			}
		}
	})

	t.Run("off, the value set anyway", func(t *testing.T) {
		chatTestEnv(t, false)
		// A path that set the variable without passing through runLaunch's
		// gate is refused by the builder itself, on every launch path.
		for _, set := range []func(){func() { slackWorkspaceID = "T1" }, func() { notifyPlatform = "teams" }} {
			slackWorkspaceID, notifyPlatform = "", ""
			set()
			if _, err := buildLaunchConfig(nil); !errors.Is(err, errChatExperimental) {
				t.Errorf("buildLaunchConfig with a chat setting and no opt-in: err = %v, want the refusal", err)
			}
		}
	})

	t.Run("on, with a stored default", func(t *testing.T) {
		home, _ := chatTestEnv(t, true)
		storeDefault(t, home)
		slackWorkspaceID, notifyPlatform = "", ""
		applyLaunchDefaults(launchCmd)
		if slackWorkspaceID != "T03NE3GTY" {
			t.Fatalf("opted in, and the stored default was not applied: %q", slackWorkspaceID)
		}
		notifyPlatform = "teams"
		cfg, body := settingsOf(t)
		if cfg.NotifyURL == "" || cfg.SlackWorkspaceID != "T03NE3GTY" || cfg.NotifyPlatform != "teams" {
			t.Errorf("opted in: url=%q workspace=%q platform=%q", cfg.NotifyURL, cfg.SlackWorkspaceID, cfg.NotifyPlatform)
		}
		for _, k := range chatSettingKeys {
			if !strings.Contains(body, `"`+k+`"`) {
				t.Errorf("opted in, and the launch config object has no %s:\n%s", k, body)
			}
		}
	})
}

// TestResumeDropsRecordedChatWithoutTheOptIn: a sweep checkpoint recorded
// with chat settings resumes without them unless opted in.
func TestResumeDropsRecordedChatWithoutTheOptIn(t *testing.T) {
	withChat := func() *SweepMemberRecord {
		return &SweepMemberRecord{Base: aws.LaunchConfig{
			InstanceType: "m7i.large", SlackWorkspaceID: "T1", NotifyURL: "https://bot.example.invalid",
			NotifyCommand: "/spore", NotifyPlatform: "teams",
		}}
	}
	chatTestEnv(t, false)
	rec := withChat()
	stripChatFromResumedBase(rec)
	if b := rec.Base; b.SlackWorkspaceID != "" || b.NotifyURL != "" || b.NotifyCommand != "" || b.NotifyPlatform != "" {
		t.Errorf("resumed without the opt-in, and the base still carries chat settings: %+v", b)
	}
	if rec.Base.InstanceType != "m7i.large" {
		t.Error("stripping chat settings touched another field")
	}
	stripChatFromResumedBase(nil) // an older checkpoint has no record

	t.Setenv(spawnconfig.ExperimentalChatEnv, "1")
	rec = withChat()
	stripChatFromResumedBase(rec)
	if rec.Base.SlackWorkspaceID != "T1" || rec.Base.NotifyURL == "" {
		t.Errorf("opted in, and the resume dropped the chat settings: %+v", rec.Base)
	}
}

// TestChatOptInNeedsTheOperatorsOwnBot: opted in, chat still makes no call to
// spore.host's retired hosted bot. With no SPORE_NOTIFY_URL a chat launch is
// refused, and with no SPORE_BOT_LAMBDA_ROLE_ARN (and no --role-arn) so is
// `notify register`, each before any AWS call; a value naming the retired bot
// is refused the same way; and nothing falls back to a built-in endpoint.
func TestChatOptInNeedsTheOperatorsOwnBot(t *testing.T) {
	resetRootOutputFlag(t)
	resetCommandFlags(t, botCmd)
	retiredURL := "https://awdzf7fbbsvqcrnrzusqjsuybm0iiyvf.lambda-url.us-east-1.on.aws"
	retiredRole := "arn:aws:iam::" + "966362334030" + ":role/" + "spore-bot-role"

	for _, notifyURL := range []string{"", retiredURL} {
		t.Run("launch, SPORE_NOTIFY_URL="+notifyURL, func(t *testing.T) {
			keepLaunchGlobals(t)
			_, activity := chatTestEnv(t, true)
			t.Setenv(spawnconfig.EnvNotifyURL, notifyURL)
			setLaunchFlag(t, "instance-type", "t4g.small")
			setLaunchFlag(t, "slack-workspace", "T1")
			err := runLaunch(launchCmd, []string{"box"})
			if err == nil || !strings.Contains(err.Error(), spawnconfig.EnvNotifyURL) {
				t.Fatalf("err = %v, want a refusal naming %s", err, spawnconfig.EnvNotifyURL)
			}
			if loads, reqs := activity(); loads != 0 || reqs != 0 {
				t.Errorf("refused, but loaded an AWS config %d time(s) and sent %d request(s)", loads, reqs)
			}
			// The config builder refuses too, for a path that reaches it
			// without the flag (a stored default).
			if _, err := buildLaunchConfig(nil); err == nil || !strings.Contains(err.Error(), spawnconfig.EnvNotifyURL) {
				t.Errorf("buildLaunchConfig: err = %v, want a refusal naming %s", err, spawnconfig.EnvNotifyURL)
			}
		})
	}

	for _, role := range []string{"", retiredRole} {
		t.Run("notify register, SPORE_BOT_LAMBDA_ROLE_ARN="+role, func(t *testing.T) {
			_, activity := chatTestEnv(t, true)
			t.Setenv(spawnconfig.EnvBotLambdaRoleARN, role)
			err := runRootForTest(t, "notify", "register", "--platform", "slack", "--instance", "i-0123456789abcdef0",
				"--user-id", "U1", "--workspace-id", "T1")
			if err == nil || !strings.Contains(err.Error(), spawnconfig.EnvBotLambdaRoleARN) {
				t.Fatalf("err = %v, want a refusal naming %s", err, spawnconfig.EnvBotLambdaRoleARN)
			}
			if loads, reqs := activity(); loads != 0 || reqs != 0 {
				t.Errorf("refused, but loaded an AWS config %d time(s) and sent %d request(s)", loads, reqs)
			}
		})
	}

	t.Run("notify register --role-arn needs no bot principal", func(t *testing.T) {
		_, activity := chatTestEnv(t, true)
		t.Setenv(spawnconfig.EnvBotLambdaRoleARN, "")
		err := runRootForTest(t, "notify", "register", "--platform", "slack", "--instance", "i-0123456789abcdef0",
			"--user-id", "U1", "--workspace-id", "T1", "--role-arn", "arn:aws:iam::111122223333:role/SpawnBotCrossAccount", "--external-id", "x")
		if err != nil && strings.Contains(err.Error(), spawnconfig.EnvBotLambdaRoleARN) {
			t.Errorf("--role-arn was given, and register still asked for the bot principal: %v", err)
		}
		if loads, _ := activity(); loads == 0 {
			t.Errorf("register with --role-arn never reached AWS (err: %v)", err)
		}
	})
}
