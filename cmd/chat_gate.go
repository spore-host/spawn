package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// The chat-platform wall (spore-host/spore-host#611, #644).
//
// Slack, Teams and Discord notifications (and spore-bot's SMS, which rides the
// same registrations) are experimental and unsupported in this release. The
// code is kept so the feature can return after 1.0, but every user-facing entry
// point requires SPAWN_EXPERIMENTAL_CHAT=1 (or experimental.chat: true in
// ~/.spawn/config.yaml) first. Without it they are hidden from help and refuse,
// before any AWS call:
//
//   - the whole `spawn notify` group (alias `bot`), including the deprecated
//     flat `notify workspace-*` shims;
//   - `spawn launch --slack-workspace` and `--notify-platform`;
//   - `spawn defaults set slack-workspace`.
//
// A `spawn sweep resume` of a sweep whose checkpoint records chat settings
// drops them, with a notice, unless opted in (stripChatFromResumedBase).
//
// And on the launch path nothing chat-related reaches an instance: a stored
// defaults.slack_workspace is ignored with a notice, and buildLaunchConfig
// refuses to write a chat setting, so neither the launch config object nor any
// tag carries a notify URL, workspace or platform for spored to act on.

// errChatExperimental is the refusal every gated entry point returns.
var errChatExperimental = errors.New(spawnconfig.ExperimentalChatRefusal)

// chatEnabled is the opt-in check. A variable so a test can tell "the gate was
// consulted" from "the gate was skipped".
var chatEnabled = spawnconfig.ChatEnabled

// requireChat returns errChatExperimental unless the chat integrations are
// opted in.
func requireChat() error {
	if chatEnabled() {
		return nil
	}
	return errChatExperimental
}

// chatLaunchFlags are the `spawn launch` flags that configure chat
// notifications on an instance.
var chatLaunchFlags = []string{"slack-workspace", "notify-platform"}

// chatDefaultKeys are the `spawn defaults` keys that configure chat
// notifications.
var chatDefaultKeys = map[string]bool{"slack-workspace": true}

// chatLaunchFlagsGate refuses a launch that sets any chat flag without the
// opt-in. Called first in runLaunch, ahead of every AWS call and dispatch.
//
// Opted in, a --slack-workspace also needs the operator's own spore-bot
// (SPORE_NOTIFY_URL): spore.host no longer hosts one, so that is checked here
// too, rather than after the launch's first AWS calls.
func chatLaunchFlagsGate(cmd *cobra.Command) error {
	for _, name := range chatLaunchFlags {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			if err := requireChat(); err != nil {
				return fmt.Errorf("--%s: %w", name, err)
			}
		}
	}
	if f := cmd.Flags().Lookup("slack-workspace"); f != nil && f.Changed && f.Value.String() != "" {
		if _, err := spawnconfig.ResolveNotifyURL(); err != nil {
			return fmt.Errorf("--slack-workspace: %w", err)
		}
	}
	return nil
}

// applyChatVisibility hides the chat entry points from help unless they are
// opted in. Called from init (hidden: the default) and from Execute, once the
// environment and config file can be read.
func applyChatVisibility(visible bool) {
	botCmd.Hidden = !visible
	for _, name := range chatLaunchFlags {
		if f := launchCmd.Flags().Lookup(name); f != nil {
			f.Hidden = !visible
		}
	}
	defaultsCmd.Long = defaultsLongHelp(visible)
}

// chatGatePreRun is the `spawn notify` group's PersistentPreRunE. Root's runs
// first (cobra.EnableTraverseRunHooks), then this, then the subcommand's RunE,
// which is the first thing that loads an AWS config.
func chatGatePreRun(cmd *cobra.Command, args []string) error {
	return requireChat()
}

// warnIgnoredChatDefault says, once per launch, that a stored chat default is
// not being applied.
func warnIgnoredChatDefault(key, value string) {
	fmt.Fprintf(os.Stderr, "ℹ️  ignoring defaults.%s (%s) in ~/.spawn/config.yaml: %s\n", key, value, spawnconfig.ExperimentalChatRefusal)
}

// clearChatSettings blanks every chat setting in c and reports whether any
// was set.
func clearChatSettings(c *aws.LaunchConfig) bool {
	had := c.SlackWorkspaceID != "" || c.NotifyURL != "" || c.NotifyCommand != "" || c.NotifyPlatform != ""
	c.SlackWorkspaceID, c.NotifyURL, c.NotifyCommand, c.NotifyPlatform = "", "", "", ""
	return had
}

// stripChatFromResumedBase drops the chat settings a sweep checkpoint recorded,
// unless chat is opted in, so a resume configures no chat notifications on the
// members it relaunches. Nil-safe: an older checkpoint has no member record.
func stripChatFromResumedBase(member *SweepMemberRecord) {
	if member == nil || chatEnabled() {
		return
	}
	if clearChatSettings(&member.Base) {
		fmt.Fprintf(os.Stderr, "ℹ️  this sweep was launched with chat notifications; resuming without them: %s\n", spawnconfig.ExperimentalChatRefusal)
	}
}
