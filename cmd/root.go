package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/libs/update"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/buildinfo"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/launcher"
)

// Version is the release version, injected at build time by GoReleaser
// (-X .../cmd.Version={{.Version}}).
//
// The zero value is deliberately empty rather than a hand-written number. A
// hardcoded default is a second source of truth that nothing updates: this said
// "0.38.1" while the current release was v0.97.0, so every source build — and
// anything reading `spawn version` — reported a release from 59 versions earlier,
// confidently and wrongly. An empty value instead falls back to the build
// metadata Go stamps automatically (see resolveVersion), which cannot go stale
// because nobody maintains it.
var Version = ""

// Global flags for i18n and accessibility
var (
	flagLang          string
	flagNoEmoji       bool
	flagAccessibility bool
	flagNoColor       bool

	// Output / display flags
	spawnOutputFormat string
	spawnVerbose      bool

	// Shared spore.host config flags (see libs/sporeconfig).
	spawnProfile string
	spawnRegion  string
	spawnAccount string
)

var rootCmd = &cobra.Command{
	Use: "spawn",
	// Short and Long descriptions will be set after i18n initialization

	// Execute() prints the error and exits non-zero itself, so let cobra not
	// also print it (avoids the duplicate "Error: ..." line), and don't dump the
	// usage wall on a runtime RunE failure — usage is for `--help`/misuse, not
	// for "file not found". Both are inherited by every subcommand.
	SilenceErrors: true,
	SilenceUsage:  true,
}

var i18nInitialized = false

func Execute() {
	// Parse flags early to get --lang value before help is displayed
	_ = rootCmd.ParseFlags(os.Args[1:])
	ensureI18nInitialized()

	// So every launched instance's spawn:version tag reflects the ACTUAL
	// running spawn, not a restated literal (spawn#515) — pkg/aws can't
	// import cmd to resolve this itself, so it's pushed in here once at
	// startup.
	aws.CallerVersion = version()
	// And so a release build's user data fetches the bootstrap body published
	// for exactly this release (#1274); pkg/launcher cannot import cmd either.
	launcher.CallerVersion = version()

	// Start async update check (non-blocking, respects SPORE_NO_UPDATE_CHECK).
	updateCh := startUpdateCheck(version())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// A declined prompt exits with its own code, so a caller can tell
		// "you said no" from "it failed" without parsing text (#737).
		os.Exit(exitForError(err))
	}

	// Print update notice after command completes (if available)
	select {
	case result := <-updateCh:
		if result.HasUpdate() {
			fmt.Fprintf(os.Stderr, "\n%s\n", result.Message())
		}
	default:
	}
}

// startUpdateCheck kicks off the background release check for the resolved
// version, or returns an already-closed channel for a dev build.
//
// Skipping dev builds is not cosmetic: libs' semver parser reads "dev" as 0.0.0,
// so a source build would compare 0.0.0 against the newest release and nag on
// every single command — including when the build is actually AHEAD of the last
// release, which is the normal state while developing. The old hardcoded 0.38.1
// had the same effect for a less visible reason.
func startUpdateCheck(current string) <-chan *update.Result {
	if buildinfo.IsDev(current) {
		ch := make(chan *update.Result)
		close(ch) // a receive returns nil immediately; HasUpdate() is nil-safe
		return ch
	}
	return checkAsync("spawn", current)
}

// checkAsync is update.CheckAsync behind a seam, so a test can observe WHETHER
// the check was delegated. Without it, "skipped for a dev build" and "delegated
// for a real version" are indistinguishable from the outside — both hand back a
// channel that yields immediately — and a guard widened to skip every build would
// pass the test while silently disabling the update notice for everyone.
var checkAsync = update.CheckAsync

func init() {
	// Route Go `log` diagnostics off the terminal unless --verbose (#895).
	// OnInitialize rather than root's PersistentPreRunE: it runs after flag
	// parsing for EVERY command, including those (`spawn app ...`) that declare
	// their own PersistentPreRunE and so never run root's.
	cobra.OnInitialize(configureDiagnosticLog)

	// Run every ancestor's PersistentPreRunE, not only the nearest one. Cobra's
	// default runs just the closest hook, so a group that declares its own (as
	// `spawn app` does for --catalog) silently replaced this root hook for its
	// whole subtree: --profile/--region/--account never reached the config
	// layer and --output was never validated there (#975).
	cobra.EnableTraverseRunHooks = true

	// Set PersistentPreRunE to initialize i18n after flag parsing
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		ensureI18nInitialized()
		// Reject an --output value no command renders, before anything runs:
		// otherwise a typo or a file name falls through to the table (#975).
		if err := validateOutputFormat(cmd); err != nil {
			return err
		}
		// Push the shared-config flag values into the config layer so
		// pkg/config (and pkg/aws through it) can resolve profile/region with
		// flag > env > file > default precedence.
		spawnconfig.SetSharedFlags(spawnProfile, spawnRegion, spawnAccount)
		return nil
	}

	// Add global i18n and accessibility flags
	rootCmd.PersistentFlags().StringVar(&flagLang, "lang", "", "Language for output (en, es, fr, de, ja, pt)")
	rootCmd.PersistentFlags().BoolVar(&flagNoEmoji, "no-emoji", false, "Disable emoji in output")
	rootCmd.PersistentFlags().BoolVar(&flagAccessibility, "accessibility", false, "Enable accessibility mode (implies --no-emoji)")
	rootCmd.PersistentFlags().BoolVar(&flagNoColor, "no-color", false, "Disable colorized output")

	// Output format and verbosity
	rootCmd.PersistentFlags().StringVarP(&spawnOutputFormat, "output", "o", outputFormatTable, "Output format ("+strings.Join(outputFormats, ", ")+")")
	rootCmd.PersistentFlags().BoolVarP(&spawnVerbose, "verbose", "v", false, "Enable verbose output (also copies the audit trail and diagnostic log lines, normally kept in ~/.spawn/audit.log and ~/.spawn/spawn.log, to stderr)")

	// Shared spore.host config (libs/sporeconfig): AWS profile/region/account,
	// resolved flag > env (SPORE_*/AWS_*) > ~/.config/spore/config.toml > default.
	// Unset = ambient AWS chain (unchanged behavior). spawn's infra/compute
	// two-account split layers on top of the resolved profile.
	rootCmd.PersistentFlags().StringVar(&spawnProfile, "profile", "", "AWS named profile (overrides SPORE_PROFILE/AWS_PROFILE and the shared config)")
	rootCmd.PersistentFlags().StringVar(&spawnRegion, "region", "", "Default AWS region (overrides SPORE_REGION/AWS_REGION and the shared config)")
	rootCmd.PersistentFlags().StringVar(&spawnAccount, "account", "", "Expected AWS account ID (optional guard)")

	// Enable shell completion for all supported shells
	rootCmd.CompletionOptions.DisableDefaultCmd = false
	rootCmd.CompletionOptions.DisableDescriptions = false
}

func ensureI18nInitialized() {
	if i18nInitialized {
		return
	}
	initI18n()
	i18nInitialized = true
}

func initI18n() {
	// Initialize i18n with configuration from flags
	cfg := i18n.Config{
		Language:          flagLang,
		Verbose:           false,
		AccessibilityMode: flagAccessibility,
		NoEmoji:           flagNoEmoji,
	}

	if err := i18n.Init(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to initialize i18n: %v\n", err)
		// Continue with default English
	}

	// Set command descriptions after i18n is initialized
	updateCommandDescriptions()
}

// getOutputFormat returns the global output format flag value.
func getOutputFormat() string {
	return spawnOutputFormat
}

func updateCommandDescriptions() {
	// Root command
	rootCmd.Short = i18n.T("spawn.root.short")
	rootCmd.Long = i18n.T("spawn.root.long")

	// Launch command
	if cmd, _, err := rootCmd.Find([]string{"launch"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.launch.short")
		cmd.Long = i18n.T("spawn.launch.long")
	}

	// Connect command
	if cmd, _, err := rootCmd.Find([]string{"connect"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.connect.short")
		cmd.Long = i18n.T("spawn.connect.long")
	}

	// List command
	if cmd, _, err := rootCmd.Find([]string{"list"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.list.short")
		cmd.Long = i18n.T("spawn.list.long")
	}

	// DNS command. The long text is spawn's own: libs' spawn.dns.long says names
	// are registered "when launching with --dns flag" (they are registered from
	// --name by default) and that `spawn dns delete` takes a DNS name (it takes
	// an instance), in every language (#1159).
	if cmd, _, err := rootCmd.Find([]string{"dns"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.dns.short")
		cmd.Long = dnsLongHelp
	}
	// Its subcommands: the keys exist in libs and were never applied, so
	// `spawn dns --help` listed all three with blank descriptions (#1159).
	for _, sub := range []string{"list", "register", "delete"} {
		if cmd, _, err := rootCmd.Find([]string{"dns", sub}); err == nil && cmd != nil && cmd.Name() == sub {
			cmd.Short = i18n.T("spawn.dns." + sub + ".short")
		}
	}

	// Extend command
	if cmd, _, err := rootCmd.Find([]string{"extend"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.extend.short")
		cmd.Long = i18n.T("spawn.extend.long")
	}

	// State command
	if cmd, _, err := rootCmd.Find([]string{"state"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.state.short")
		cmd.Long = i18n.T("spawn.state.long")
	}
}
